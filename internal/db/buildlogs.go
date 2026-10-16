package db

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"github.com/FanDoster/Build-System/internal/models"
	"time"
)

// Chunks are bounded independently of transport uploads. Offsets and lengths
// are bytes, including when a slice starts inside a UTF-8 rune.
const buildLogChunkBytes = 64 * 1024

func (d *DB) migrateBuildLogs() error {
	_, err := d.conn.Exec(`
 CREATE TABLE IF NOT EXISTS build_log_state (
  build_id INTEGER PRIMARY KEY REFERENCES builds(id) ON DELETE CASCADE,
  legacy_bytes INTEGER NOT NULL CHECK(legacy_bytes >= 0),
  total_bytes INTEGER NOT NULL CHECK(total_bytes >= legacy_bytes)
 );
 CREATE TABLE IF NOT EXISTS build_log_chunks (
  build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  byte_offset INTEGER NOT NULL CHECK(byte_offset >= 0),
  data BLOB NOT NULL CHECK(length(data) > 0 AND length(data) <= 65536),
  PRIMARY KEY(build_id, byte_offset)
 ) WITHOUT ROWID;
 `)
	return err
}

// logBounds never copies legacy log content. Existing rows have no state until
// their first append. Their immutable builds.log remains the prefix forever;
// migration neither scans nor duplicates historical logs.
func logBounds(tx *sql.Tx, id int64) (legacy, total int, err error) {
	err = tx.QueryRow(`SELECT legacy_bytes, total_bytes FROM build_log_state WHERE build_id=?`, id).Scan(&legacy, &total)
	if err == sql.ErrNoRows {
		err = tx.QueryRow(`SELECT length(CAST(log AS BLOB)) FROM builds WHERE id=?`, id).Scan(&legacy)
		total = legacy
	}
	return
}

func appendBuildLog(tx *sql.Tx, id int64, data []byte) (int, error) {
	var total int
	err := tx.QueryRow(`SELECT total_bytes FROM build_log_state WHERE build_id=?`, id).Scan(&total)
	if err == sql.ErrNoRows {
		if err = tx.QueryRow(`SELECT length(CAST(log AS BLOB)) FROM builds WHERE id=?`, id).Scan(&total); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`INSERT INTO build_log_state(build_id,legacy_bytes,total_bytes) VALUES(?,?,?)`, id, total, total); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}

	start := total
	for len(data) > 0 {
		n := len(data)
		if n > buildLogChunkBytes {
			n = buildLogChunkBytes
		}
		if _, err = tx.Exec(`INSERT INTO build_log_chunks(build_id, byte_offset, data) VALUES(?,?,?)`, id, total, data[:n]); err != nil {
			return 0, err
		}
		total += n
		data = data[n:]
	}
	_, err = tx.Exec(`UPDATE build_log_state SET total_bytes=? WHERE build_id=?`, total, id)
	return start, err
}

// AppendBuildLogAt returns the durable starting byte offset. Callers must commit
// before publishing to the bus: a failed write must never become a live byte.
func (d *DB) AppendBuildLogAt(id int64, line string) (int, error) {
	tx, err := d.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	start, err := appendBuildLog(tx, id, []byte(line))
	if err != nil {
		return 0, err
	}
	return start, tx.Commit()
}

func (d *DB) AppendBuildLog(id int64, line string) error {
	_, err := d.AppendBuildLogAt(id, line)
	return err
}

func (d *DB) BuildLogLen(id int64) (int, error) {
	var n int
	err := d.conn.QueryRow(`SELECT total_bytes FROM build_log_state WHERE build_id=?`, id).Scan(&n)
	if err == sql.ErrNoRows {
		err = d.conn.QueryRow(`SELECT length(CAST(log AS BLOB)) FROM builds WHERE id=?`, id).Scan(&n)
	}
	return n, err
}

// BuildLogSlice reads only the requested legacy prefix bytes and intersecting
// chunks. The predecessor lookup and range scan use the composite primary key;
// a byte tail never walks earlier chunks. A transaction pins one log snapshot.
func (d *DB) BuildLogSlice(id int64, offset, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative log offset")
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	legacy, total, err := logBounds(tx, id)
	if err != nil {
		return nil, err
	}
	if offset >= total {
		return nil, nil
	}
	if n > total-offset {
		n = total - offset
	}
	end := offset + n
	out := make([]byte, 0, n)
	if offset < legacy {
		count := legacy - offset
		if count > n {
			count = n
		}
		var prefix []byte
		if err = tx.QueryRow(`SELECT substr(CAST(log AS BLOB),?,?) FROM builds WHERE id=?`, offset+1, count, id).Scan(&prefix); err != nil {
			return nil, err
		}
		out = append(out, prefix...)
	}
	start := offset
	if start < legacy {
		start = legacy
	}
	if start < end {
		rows, err := tx.Query(`SELECT byte_offset, substr(data, max(0, ?-byte_offset)+1, min(length(data), ?-byte_offset)-max(0, ?-byte_offset))
   FROM build_log_chunks WHERE build_id=? AND byte_offset >= COALESCE(
    (SELECT MAX(byte_offset) FROM build_log_chunks WHERE build_id=? AND byte_offset<=?), ?)
    AND byte_offset<? ORDER BY byte_offset`, start, end, start, id, id, start, start, end)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var pos int
			var chunk []byte
			if err = rows.Scan(&pos, &chunk); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, chunk...)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DB) hydrateBuildLogs(builds []models.Build) ([]models.Build, error) {
	for i := range builds {
		n, err := d.BuildLogLen(builds[i].ID)
		if err != nil {
			return nil, err
		}
		data, err := d.BuildLogSlice(builds[i].ID, 0, n)
		if err != nil {
			return nil, err
		}
		builds[i].Log = string(data)
	}
	return builds, nil
}

var ErrLogConflict = errors.New("build ownership, status or log length changed")

// AppendAgentBuildLog rechecks the lease and length in the append transaction.
// The janitor may run concurrently with an HTTP upload; bytes cannot land after
// its terminal tombstone or on a different owner's build.
func (d *DB) AppendAgentBuildLog(id int64, agent string, expected int, data string) (int, error) {
	tx, err := d.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var status, owner string
	if err = tx.QueryRow(`SELECT status,agent FROM builds WHERE id=?`, id).Scan(&status, &owner); err != nil {
		return 0, err
	}
	_, total, err := logBounds(tx, id)
	if err != nil {
		return 0, err
	}
	if status != string(models.StatusRunning) || owner != agent || total != expected {
		return total, ErrLogConflict
	}
	start, err := appendBuildLog(tx, id, []byte(data))
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE builds SET last_heartbeat_at=? WHERE id=?`, time.Now().UTC(), id); err != nil {
		return 0, err
	}
	return start, tx.Commit()
}

// Startup compatibility repair scans in bounded windows, including markers
// crossing chunk boundaries, rather than assembling every historical log.
func (d *DB) buildLogContains(id int64, marker string) (bool, error) {
	n, err := d.BuildLogLen(id)
	if err != nil {
		return false, err
	}
	var carry []byte
	for offset := 0; offset < n; offset += buildLogChunkBytes {
		part, err := d.BuildLogSlice(id, offset, buildLogChunkBytes)
		if err != nil {
			return false, err
		}
		window := append(carry, part...)
		if bytes.Contains(window, []byte(marker)) {
			return true, nil
		}
		keep := len(marker) - 1
		if keep > len(window) {
			keep = len(window)
		}
		carry = append(carry[:0], window[len(window)-keep:]...)
	}
	return false, nil
}

// FailPendingBuildWithNote is the queue-full path. It preserves every byte of
// previous attempts, unlike the historical full-log replacement API.
func (d *DB) FailPendingBuildWithNote(id int64, note string) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE builds SET status=?,finished_at=? WHERE id=? AND status=?`, models.StatusFailed, time.Now().UTC(), id, models.StatusPending)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if _, err = appendBuildLog(tx, id, []byte(note)); err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueBuildWithNote commits the seam and state change together. Callers
// first flush their sink and publish this returned offset only after success.
func (d *DB) RequeueBuildWithNote(id int64, maxRequeues int) (bool, int, error) {
	tx, err := d.conn.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE builds SET status=?,requeues=requeues+1,started_at=NULL,finished_at=NULL WHERE id=? AND status=? AND requeues<? AND agent=''`, models.StatusPending, id, models.StatusRunning, maxRequeues)
	if err != nil {
		return false, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, 0, err
	}
	start, err := appendBuildLog(tx, id, []byte(RequeueNote))
	if err != nil {
		return false, 0, err
	}
	return true, start, tx.Commit()
}

// FinishAgentBuild lets exactly one terminal decision win against the janitor.
func (d *DB) FinishAgentBuild(id int64, agent string, status models.BuildStatus) (bool, error) {
	res, err := d.conn.Exec(`UPDATE builds SET status=?,finished_at=? WHERE id=? AND status=? AND agent=?`, status, time.Now().UTC(), id, models.StatusRunning, agent)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
