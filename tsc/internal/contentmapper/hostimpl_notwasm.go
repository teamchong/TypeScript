//go:build !wasm

package contentmapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"github.com/microsoft/TypeScript/tsc/internal/locale"
)

func dialMapperProcess(logger Logger, spawner Spawner, timing *timingCollector) dialFunc {
	return func(ctx context.Context, mapper *Mapper, diagnosticLocale locale.Locale) (ipc.Conn, io.Closer, PositionEncoding, string, error) {
		if len(mapper.Exec) == 0 {
			return nil, nil, "", "", fmt.Errorf("content mapper %q declares no command to run", mapper.Package)
		}
		mapperTiming := timing.mapper(mapper.Identity())
		diagnosticName := mapper.DiagnosticName()
		spawnStart := time.Now()
		var stderr io.Writer = io.Discard
		var stderrLog *stderrLogger
		if logger != nil {
			stderrLog = &stderrLogger{mapperName: diagnosticName, logger: logger}
			stderr = stderrLog
		}
		rwc, err := spawner.Spawn(mapper.Exec, mapper.PackageDirectory.AsString(), stderr)
		mapperTiming.spawn.record(spawnStart)
		if err != nil {
			return nil, nil, "", "", &InitializeError{Kind: InitializeErrorKindProcessStart, MapperName: diagnosticName, Command: mapper.Exec[0], Detail: err.Error()}
		}
		if stderrLog != nil {
			rwc = &loggedProcess{ReadWriteCloser: rwc, stderr: stderrLog}
		}
		rwc = &closeOnceReadWriteCloser{ReadWriteCloser: rwc}
		protocol := ipc.Protocol(ipc.NewJSONRPCProtocol(rwc))
		if logger != nil {
			protocol = &loggingProtocol{Protocol: protocol, mapperName: diagnosticName, logger: logger}
		}
		conn := ipc.NewAsyncConnWithProtocol(rwc, protocol, rejectHandler{})
		go func() {
			_ = conn.Run(ctx)
			_ = rwc.Close()
		}()
		initializeCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
		initializeStart := mapperTiming.startRequest()
		positionEncoding, diagnosticSource, err := handshake(initializeCtx, conn, diagnosticLocale)
		mapperTiming.finishRequest(&mapperTiming.initialize, initializeStart)
		initializeCtxErr := initializeCtx.Err()
		cancel()
		if err != nil {
			var exitCode int
			var exited bool
			if exitState, ok := rwc.(processExitState); ok {
				exitCode, exited = exitState.ExitCode()
			}
			_ = rwc.Close()
			if initializeError, ok := errors.AsType[*InitializeError](err); ok {
				initializeError.MapperName = diagnosticName
				return nil, nil, "", "", initializeError
			}
			if exited {
				return nil, nil, "", "", &InitializeError{Kind: InitializeErrorKindProcessExit, MapperName: diagnosticName, ExitCode: exitCode}
			}
			if initializeCtxErr != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, "", "", &InitializeError{Kind: InitializeErrorKindNoResponse, MapperName: diagnosticName, TimeoutSeconds: initializeTimeoutSeconds}
			}
			return nil, nil, "", "", &InitializeError{Kind: InitializeErrorKindRequest, MapperName: diagnosticName, Detail: err.Error()}
		}
		return conn, rwc, positionEncoding, diagnosticSource, nil
	}
}

func newWithDial(ctx context.Context, diagnosticLocale locale.Locale, timing *timingCollector, dial dialFunc) *host {
	hostCtx, cancel := context.WithCancel(ctx)
	h := &host{ctx: hostCtx, cancel: cancel, dial: dial, timing: timing, diagnosticLocale: diagnosticLocale, conns: make(map[string]*mapperConn), projects: make(map[string]*projectEntry), projectLeases: make(map[string]*projectLease)}
	h.stop = context.AfterFunc(ctx, func() { _ = h.Close() })
	return h
}
