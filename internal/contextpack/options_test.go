package contextpack_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/chux0519/memspace/internal/contextpack"
)

func TestOptionsPreserveDefaultAndBoundItems(t *testing.T) {
	f := setup(t)
	history(f)
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: webapp, Query: "刷新令牌的测试为什么失败"}
	base := f.build("linux-box", req)
	same, e := contextpack.BuildWithOptions(context.Background(), f.db, owner, "linux-box", req, contextpack.BuildOptions{})
	if e != nil || !reflect.DeepEqual(base, same) {
		t.Fatal("default changed")
	}
	bounded, e := contextpack.BuildWithOptions(context.Background(), f.db, owner, "linux-box", req, contextpack.BuildOptions{MaxItems: 1})
	if e != nil || len(bounded.Items) > 1 {
		t.Fatal("item budget ignored")
	}
	if _, e = contextpack.BuildWithOptions(context.Background(), f.db, owner, "linux-box", req, contextpack.BuildOptions{MaxItems: -1}); e == nil {
		t.Fatal("negative accepted")
	}
}
