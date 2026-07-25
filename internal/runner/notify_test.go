package runner

import (
	"bufio"
	"fmt"
	"mime"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

// fakeSMTP is a listener that speaks just enough SMTP to accept one message.
// It advertises NO extensions — in particular no STARTTLS — which is exactly
// what the real path relies on: the client must never try to upgrade.
type fakeSMTP struct {
	addr     string
	received chan string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	f := &fakeSMTP{addr: ln.Addr().String(), received: make(chan string, 4)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	fmt.Fprint(conn, "220 fake ESMTP\r\n")

	var envelope, body strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// Single-line greeting = no extension list = no STARTTLS offered.
			fmt.Fprint(conn, "250 fake\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			envelope.WriteString(strings.TrimSpace(line) + "\n")
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			fmt.Fprint(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" {
					break
				}
				body.WriteString(dl)
			}
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			fmt.Fprint(conn, "221 Bye\r\n")
			f.received <- envelope.String() + "\n" + body.String()
			return
		case strings.HasPrefix(cmd, "STARTTLS"):
			// Never expected — the client must not attempt an upgrade.
			f.received <- "UNEXPECTED STARTTLS"
			return
		default:
			fmt.Fprint(conn, "250 OK\r\n")
		}
	}
}

func (f *fakeSMTP) await(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-f.received:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the relay")
		return ""
	}
}

func notifyFixture(t *testing.T) (*Runner, *fakeSMTP, *models.Build) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	project := &models.Project{
		Name: "windows-fpl", RepoURL: "https://github.com/u/wfpl", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "windows-fpl",
	}
	if err := database.CreateProject(project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	build := &models.Build{
		ProjectID: project.ID, Status: models.StatusPending,
		CommitSHA: "1bfe15b9", CommitMessage: "Polled: new commit on main",
	}
	if err := database.CreateBuild(build); err != nil {
		t.Fatalf("create build: %v", err)
	}

	relay := newFakeSMTP(t)
	r := New(database, make(chan *models.Build), logbus.New())
	r.NotifyEmail = "danfoster@fandoster.com"
	r.PublicURL = "https://fandoster.com/builds"
	r.SMTPAddr = relay.addr
	return r, relay, build
}

func TestNotifySendsCompletionMail(t *testing.T) {
	r, relay, build := notifyFixture(t)

	started := time.Now().UTC().Add(-52 * time.Second)
	r.notify(build.ID, models.StatusSuccess, started, started.Add(52*time.Second))

	msg := relay.await(t)
	if strings.Contains(msg, "UNEXPECTED STARTTLS") {
		t.Fatal("client attempted STARTTLS; Postfix's self-signed cert would reject it")
	}

	// Envelope: the sender must stay on the SPF-authorised domain.
	if !strings.Contains(msg, "MAIL FROM:<builds@fandoster.com>") {
		t.Errorf("wrong envelope sender in:\n%s", msg)
	}
	if !strings.Contains(msg, "RCPT TO:<danfoster@fandoster.com>") {
		t.Errorf("wrong envelope recipient in:\n%s", msg)
	}

	// Subject: non-ASCII must travel as an encoded-word, and decode back to
	// the format the pipeline has always used.
	subject := headerOf(t, msg, "Subject")
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("subject is not a valid encoded-word (%q): %v", subject, err)
	}
	if want := "[builds] windows-fpl ✔ SUCCEEDED (52s)"; decoded != want {
		t.Errorf("subject = %q, want %q", decoded, want)
	}

	for _, want := range []string{
		"Project:  windows-fpl",
		"Status:   SUCCEEDED",
		"Commit:   1bfe15b9 — Polled: new commit on main",
		"Duration: 52s",
		fmt.Sprintf("View:     https://fandoster.com/builds/builds/%d", build.ID),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("body missing %q in:\n%s", want, msg)
		}
	}
}

func TestNotifyStatusVariants(t *testing.T) {
	cases := []struct {
		status models.BuildStatus
		want   string
	}{
		{models.StatusSuccess, "[builds] windows-fpl ✔ SUCCEEDED (52s)"},
		{models.StatusFailed, "[builds] windows-fpl ✖ FAILED (52s)"},
		{models.StatusCanceled, "[builds] windows-fpl ⊘ CANCELED (52s)"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			r, relay, build := notifyFixture(t)
			started := time.Now().UTC().Add(-52 * time.Second)
			r.notify(build.ID, tc.status, started, started.Add(52*time.Second))

			subject := headerOf(t, relay.await(t), "Subject")
			decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
			if err != nil {
				t.Fatalf("decode subject: %v", err)
			}
			if decoded != tc.want {
				t.Errorf("subject = %q, want %q", decoded, tc.want)
			}
		})
	}
}

func TestNotifyDisabledWithoutAddress(t *testing.T) {
	r, relay, build := notifyFixture(t)
	r.NotifyEmail = ""

	started := time.Now().UTC()
	r.notify(build.ID, models.StatusSuccess, started, started.Add(time.Second))

	select {
	case msg := <-relay.received:
		t.Fatalf("sent mail with no recipient configured:\n%s", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNotifyRelayFailureIsNotFatal(t *testing.T) {
	// A dead relay must never take the build down with it: notify swallows
	// the error, and finish() has already written the terminal row.
	r, _, build := notifyFixture(t)
	r.SMTPAddr = "127.0.0.1:1" // nothing listening

	done := make(chan struct{})
	go func() {
		defer close(done)
		started := time.Now().UTC()
		r.notify(build.ID, models.StatusSuccess, started, started.Add(time.Second))
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("notify hung on an unreachable relay")
	}
}

func TestNotificationOmitsLinkWithoutPublicURL(t *testing.T) {
	build := &models.Build{ID: 7, ProjectName: "app", CommitSHA: "abc", CommitMessage: "m"}
	msg := notification(build, models.StatusSuccess, 3*time.Second,
		"builds@fandoster.com", "to@example.com", "", time.Now())
	if strings.Contains(msg, "View:") {
		t.Errorf("emitted a View line with no public URL:\n%s", msg)
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{52 * time.Second, "52s"},
		{100 * time.Second, "1m40s"},
		{65 * time.Minute, "1h05m"},
		{-time.Second, "0s"},
	}
	for _, c := range cases {
		if got := shortDuration(c.d); got != c.want {
			t.Errorf("shortDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// headerOf pulls one header value out of a raw message.
func headerOf(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("no %s header in:\n%s", name, msg)
	return ""
}
