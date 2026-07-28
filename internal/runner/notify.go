package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

// Build-completion email, sent through the host's Postfix over the Docker
// bridge. Nothing here is configurable by accident: the address, the transport
// and the sender all match the relay that is already set up on the server
// (Postfix -> Google Workspace MX for fandoster.com), and the pieces that make
// that work live outside this repo:
//
//   - Postfix `mynetworks` includes the Docker bridge ranges 172.17.0.0/16 and
//     172.18.0.0/16, which is why an unauthenticated connection is accepted.
//   - A UFW rule allows the bridge interface to reach port 25.
//   - An SPF record authorises the host's IP, so the mail is not treated as
//     unsolicited.
//
// Change any of those and this stops working with no change to this file.

const (
	// defaultSMTPAddr is the host's Postfix as seen from inside the container.
	defaultSMTPAddr = "172.17.0.1:25"
	// notifyFrom must stay on the SPF-authorised domain.
	notifyFrom    = "builds@fandoster.com"
	notifyTimeout = 10 * time.Second
)

// NotifyFinished sends the completion mail for a build this process did not
// run itself — an agent's build, finished through the API. Same mail, same
// fire-and-forget contract as the worker's own path; it exists because
// finish() is unreachable for a build no worker ever touched, and a build
// being remote is no reason to stop telling anyone it finished.
func (r *Runner) NotifyFinished(buildID int64, status models.BuildStatus, startedAt, finishedAt time.Time) {
	go r.notify(buildID, status, startedAt, finishedAt)
}

// notify emails the outcome of a finished build. Best effort by design: a mail
// failure is logged and dropped, never surfaced into the build's own result —
// the build already succeeded or failed on its own merits.
func (r *Runner) notify(buildID int64, status models.BuildStatus, startedAt, finishedAt time.Time) {
	if r.NotifyEmail == "" {
		return
	}
	// Summary, not GetBuild: this runs right after a build finished and its log
	// may be enormous; none of it belongs in an email.
	build, err := r.DB.GetBuildSummary(buildID)
	if err != nil {
		log.Printf("notify: build %d: %v", buildID, err)
		return
	}
	msg := notification(build, status, finishedAt.Sub(startedAt), notifyFrom, r.NotifyEmail, r.PublicURL, finishedAt)
	if err := r.sendMail(msg); err != nil {
		log.Printf("notify: build %d: %v", buildID, err)
	}
}

// sendMail delivers one message over plain TCP.
//
// Deliberately NOT smtp.SendMail: that helper upgrades to STARTTLS whenever the
// server advertises it, and the Postfix here presents a self-signed certificate
// that Go refuses. Dialling directly and driving the client keeps the session
// in plaintext, which Postfix accepts from the bridge subnets. The hop is
// container -> host on a private bridge, and Postfix uses TLS onwards.
func (r *Runner) sendMail(msg string) error {
	addr := r.SMTPAddr
	if addr == "" {
		addr = defaultSMTPAddr
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	dialer := net.Dialer{Timeout: notifyTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	// Bound the whole session, not just the dial: a wedged peer must not pin
	// this goroutine for the life of the process.
	conn.SetDeadline(time.Now().Add(notifyTimeout))

	client, err := smtp.NewClient(conn, "fandoster.com")
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	if err := client.Mail(notifyFrom); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := client.Rcpt(r.NotifyEmail); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return client.Quit()
}

// statusLabel is the subject-line verdict: a word plus the same symbol the UI
// uses, so a phone notification reads at a glance.
func statusLabel(s models.BuildStatus) (word, symbol string) {
	switch s {
	case models.StatusSuccess:
		return "SUCCEEDED", "✔"
	case models.StatusCanceled:
		return "CANCELED", "⊘"
	default:
		return "FAILED", "✖"
	}
}

// notification renders the RFC 2822 message. Pure and self-contained so the
// format is testable without a network.
func notification(b *models.Build, status models.BuildStatus, dur time.Duration, from, to, publicURL string, now time.Time) string {
	word, symbol := statusLabel(status)
	elapsed := shortDuration(dur)

	// The subject carries non-ASCII, which belongs in an encoded-word rather
	// than raw in a header.
	subject := mime.BEncoding.Encode("utf-8",
		fmt.Sprintf("[builds] %s %s %s (%s)", b.ProjectName, symbol, word, elapsed))

	var body strings.Builder
	fmt.Fprintf(&body, "Project:  %s\n", b.ProjectName)
	fmt.Fprintf(&body, "Status:   %s\n", word)
	fmt.Fprintf(&body, "Commit:   %s — %s\n", b.CommitSHA, b.CommitMessage)
	fmt.Fprintf(&body, "Duration: %s\n", elapsed)
	if publicURL != "" {
		fmt.Fprintf(&body, "View:     %s/builds/%d\n", strings.TrimRight(publicURL, "/"), b.ID)
	}

	var msg strings.Builder
	fmt.Fprintf(&msg, "From: Build-System <%s>\r\n", from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	fmt.Fprintf(&msg, "Date: %s\r\n", now.Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	msg.WriteString("\r\n")
	// SMTP is CRLF-delimited; a bare \n in the body is not a line ending.
	msg.WriteString(strings.ReplaceAll(body.String(), "\n", "\r\n"))
	return msg.String()
}

// shortDuration formats like the UI does: "52s", "1m40s", "1h05m".
func shortDuration(d time.Duration) string {
	sec := int(d.Round(time.Second).Seconds())
	if sec < 0 {
		sec = 0
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	m, s := sec/60, sec%60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}
