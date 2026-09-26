package agent

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"
)

// extSession is one external (non-vsay) login session on the machine — an SSH login on
// Linux or an RDP/console login on Windows.
type extSession struct {
	OSUser    string
	Line      string // tty / session id — with OSUser forms the dedup key
	Protocol  string // "ssh" | "rdp" | "console"
	SourceIP  string
	LoginTime time.Time
}

func (s extSession) key() string { return s.OSUser + "\x00" + s.Line }

// startSessionMonitor polls the OS for external login sessions (SSH/RDP) and reports
// login/logout events to the backend so it can alert the machine's users and keep an
// access history. Runs for the agent's lifetime.
//
// vsay's own web-terminal PTYs don't register in utmp, so they're never seen here; only
// genuine remote logins are reported. On the FIRST scan, pre-existing sessions are
// reported with seed=true so the backend records them WITHOUT re-sending alert emails
// (they were already present before the agent (re)started).
func (a *Agent) startSessionMonitor(ctx context.Context) {
	const interval = 20 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	known := map[string]extSession{}

	report := func(action string, s extSession, ts time.Time, seed bool) {
		msg, _ := json.Marshal(map[string]interface{}{
			"action":    action,
			"protocol":  s.Protocol,
			"os_user":   s.OSUser,
			"source_ip": s.SourceIP,
			"line":      s.Line,
			"timestamp": ts.UTC().Format(time.RFC3339),
			"seed":      seed,
		})
		if err := a.grpcClient.SendStatusUpdate("__access_event__", string(msg)); err != nil {
			a.logger.Debug("session monitor: report failed", zap.Error(err))
		}
	}

	// reportSnapshot sends the complete current session list in one message.
	//
	// This is what the FIRST scan after an agent start sends, and it exists
	// because the `known` map lives only in memory. After a restart every
	// pre-existing session looked new, so the backend recorded a fresh login
	// for each one — three agent restarts produced three identical entries.
	// A snapshot lets the backend reconcile instead of blindly inserting, and
	// also lets it close sessions that ended while the agent was down, which
	// nothing else would ever have closed.
	reportSnapshot := func(sessions []extSession) {
		list := make([]map[string]interface{}, 0, len(sessions))
		for _, s := range sessions {
			list = append(list, map[string]interface{}{
				"protocol":  s.Protocol,
				"os_user":   s.OSUser,
				"source_ip": s.SourceIP,
				"line":      s.Line,
				"timestamp": s.LoginTime.UTC().Format(time.RFC3339),
			})
		}
		msg, _ := json.Marshal(map[string]interface{}{"sessions": list})
		if err := a.grpcClient.SendStatusUpdate("__access_snapshot__", string(msg)); err != nil {
			a.logger.Debug("session monitor: snapshot failed", zap.Error(err))
		}
	}

	scan := func(seed bool) {
		sessions, err := listExternalSessions()
		if err != nil {
			a.logger.Debug("session monitor: list failed", zap.Error(err))
			return
		}
		current := make(map[string]extSession, len(sessions))
		for _, s := range sessions {
			current[s.key()] = s
		}

		if seed {
			a.logger.Info("Reporting existing login sessions", zap.Int("count", len(sessions)))
			reportSnapshot(sessions)
			known = current
			return
		}

		for k, s := range current {
			if _, ok := known[k]; !ok {
				a.logger.Info("External login detected",
					zap.String("user", s.OSUser), zap.String("protocol", s.Protocol),
					zap.String("from", s.SourceIP))
				report("login", s, s.LoginTime, seed)
			}
		}
		for k, s := range known {
			if _, ok := current[k]; !ok {
				report("logout", s, time.Now(), false)
			}
		}
		known = current
	}

	scan(true) // first scan seeds existing sessions without alerting
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan(false)
		}
	}
}
