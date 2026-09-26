// Command wxt-agent-session is the session helper: the half of remote control
// that runs inside the logged-in user's desktop.
//
// It exists because the agent daemon cannot do this work itself. A Windows
// service lives in session 0 and is walled off from the interactive desktop;
// a macOS LaunchDaemon has no window server connection and can never hold the
// Screen Recording or Accessibility rights. Screen capture, input injection
// and the consent prompt therefore all have to happen here.
//
// The helper is never started by a user. The daemon launches it with a
// one-time token on the command line and it connects straight back; with no
// token, or the wrong one, it exits.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/ipc"
	"github.com/Yesveer/wxt-agent/internal/remotecontrol/session"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// version is stamped at build time with -ldflags. The daemon checks it on
// connect so a helper left behind by an older install is rejected rather than
// failing later on a message it does not understand.
var version = "dev"

// keepAlivePeriod bounds how long the helper keeps running after the daemon
// disappears without closing the connection.
const keepAlivePeriod = 30 * time.Second

func main() {
	var (
		connect = flag.String("connect", "", "address of the agent's helper listener (required)")
		token   = flag.String("token", "", "one-time authentication token (required)")
		logPath = flag.String("log", "", "write logs to this file instead of stderr")
		showVer = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("wxt-agent-session %s\n", version)
		return
	}
	if *connect == "" || *token == "" {
		fmt.Fprintln(os.Stderr,
			"wxt-agent-session is launched by the agent, not started by hand.\n"+
				"Both --connect and --token are required.")
		os.Exit(2)
	}

	logger := newLogger(*logPath)
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	helper, err := ipc.Dial(*connect, *token, version)
	if err != nil {
		logger.Error("Could not reach the agent", zap.Error(err))
		os.Exit(1)
	}
	defer func() { _ = helper.Close() }()

	logger.Info("Session helper started",
		zap.String("version", version),
		zap.String("agent", *connect),
		zap.Int("pid", os.Getpid()),
		zap.String("privilege", describeElevation()))

	svc := session.New(logger)
	svc.SetEmitter(func(e ipc.Event) {
		if err := helper.SendEvent(e); err != nil {
			logger.Debug("Could not deliver event to the agent", zap.Error(err))
		}
	})

	go helper.KeepAlive(ctx, keepAlivePeriod)

	// Serve returns when the daemon goes away. Stopping the session then is
	// the important part: an orphaned helper must not keep the user's screen
	// capturable with nobody supervising it.
	err = helper.Serve(ctx, svc)
	_ = svc.Close()

	if err != nil && ctx.Err() == nil {
		logger.Error("Connection to the agent failed", zap.Error(err))
		os.Exit(1)
	}
	logger.Info("Session helper stopped")
}

// newLogger builds a logger matching the agent's format. Falling back to
// stderr matters: the helper is launched detached, so a failure to open the
// log file must not be the reason nobody can see why it died.
func newLogger(path string) *zap.Logger {
	encCfg := zapcore.EncoderConfig{
		TimeKey:       "ts",
		LevelKey:      "level",
		NameKey:       "logger",
		MessageKey:    "msg",
		StacktraceKey: "stacktrace",
		LineEnding:    zapcore.DefaultLineEnding,
		EncodeLevel:   zapcore.CapitalLevelEncoder,
		EncodeTime: func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
			enc.AppendString(t.Format("2006-01-02 15:04:05"))
		},
		EncodeDuration:   zapcore.StringDurationEncoder,
		ConsoleSeparator: " | ",
	}

	out := zapcore.Lock(os.Stderr)
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640) // #nosec G304 -- path comes from the agent's own command line
		if err == nil {
			out = zapcore.Lock(f)
		}
	}

	return zap.New(zapcore.NewCore(zapcore.NewConsoleEncoder(encCfg), out, zapcore.InfoLevel))
}
