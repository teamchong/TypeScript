//go:build wasm

package contentmapper

import (
	"context"
	"fmt"
	"io"

	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"github.com/microsoft/TypeScript/tsc/internal/locale"
)

func dialMapperProcess(_ Logger, _ Spawner, _ *timingCollector) dialFunc {
	return func(_ context.Context, mapper *Mapper, _ locale.Locale) (ipc.Conn, io.Closer, PositionEncoding, string, error) {
		if len(mapper.Exec) == 0 {
			return nil, nil, "", "", fmt.Errorf("content mapper %q declares no command to run", mapper.Package)
		}
		return nil, nil, "", "", &InitializeError{Kind: InitializeErrorKindProcessStart, MapperName: mapper.DiagnosticName(), Command: mapper.Exec[0], Detail: "spawn not supported on wasm"}
	}
}

func newWithDial(ctx context.Context, diagnosticLocale locale.Locale, timing *timingCollector, dial dialFunc) *host {
	return &host{
		ctx:              ctx,
		cancel:           func() {},
		stop:             func() bool { return false },
		dial:             dial,
		timing:           timing,
		diagnosticLocale: diagnosticLocale,
		conns:            make(map[string]*mapperConn),
		projects:         make(map[string]*projectEntry),
		projectLeases:    make(map[string]*projectLease),
	}
}
