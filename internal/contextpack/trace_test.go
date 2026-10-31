package contextpack_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/chux0519/memspace/internal/contextpack"
)

func TestTraceDoesNotChangePackOrExposeMutablePipelineSlices(t *testing.T) {
	f := setup(t)
	history(f)
	for _, mode := range []string{contextpack.ModePrompt, contextpack.ModeExplicit, contextpack.ModeSessionStart} {
		t.Run(mode, func(t *testing.T) {
			req := contextpack.Request{Mode: mode, Project: webapp, AgentType: "claude-code", NativeSessionID: "native-current", Query: "认证刷新令牌"}
			baseline, err := contextpack.Build(context.Background(), f.db, owner, "linux-box", req)
			if err != nil {
				t.Fatal(err)
			}
			stages := map[string]bool{}
			traced, err := contextpack.BuildWithTrace(context.Background(), f.db, owner, "linux-box", req, func(event contextpack.TraceEvent) {
				stages[event.Stage] = true
				if len(event.Hits) > 0 {
					event.Hits[0].RefID = "observer mutation"
					event.Hits[0].DocID = -1
				}
				if len(event.Items) > 0 {
					event.Items[0].Ref = "observer mutation"
				}
			})
			if err != nil || !reflect.DeepEqual(baseline, traced) {
				t.Fatalf("observer changed pack: %v\n%+v\n%+v", err, baseline, traced)
			}
			if !stages["rendered"] {
				t.Fatal("missing final output")
			}
			if mode != contextpack.ModeSessionStart {
				for _, s := range []string{"search", "deduplicated", "admitted", "ordered", "content", "selected", "sections"} {
					if !stages[s] {
						t.Errorf("missing stage %s", s)
					}
				}
			}
		})
	}
}
