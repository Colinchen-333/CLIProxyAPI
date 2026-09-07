package helps

import (
	"context"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/requestmeta"
)

func TestOwnPromptCacheConcurrentColdStartStableAndScoped(t *testing.T) {
	ctx := requestmeta.WithOwnSession(context.Background(), "opaque-own-session")
	ids := make(chan string, 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, ok, err := ClaudeCodePromptCache(ctx, "muse-spark-1.3-contributor", nil, nil)
			if err != nil || !ok {
				t.Error("missing own cache key")
			}
			ids <- c.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := <-ids
	for id := range ids {
		if id != first {
			t.Fatal("concurrent first requests changed cache key")
		}
	}
	other, _, _ := ClaudeCodePromptCache(ctx, "different-model", nil, nil)
	if first == "" || other.ID == first {
		t.Fatal("model isolation missing")
	}
	other, _, _ = ClaudeCodePromptCache(requestmeta.WithOwnSession(context.Background(), "different-session"), "muse-spark-1.3-contributor", nil, nil)
	if other.ID == first {
		t.Fatal("session isolation missing")
	}
	_, ok, err := ClaudeCodePromptCache(context.Background(), "muse-spark-1.3-contributor", nil, nil)
	if err != nil || ok {
		t.Fatal("must not change requests without trusted own metadata")
	}
}

func TestOwnPromptCacheSurvivesDetachedHandlerContext(t *testing.T) {
	reqCtx := requestmeta.WithOwnSession(context.Background(), "opaque-session")
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(reqCtx)
	detached := context.WithValue(context.Background(), "gin", ginCtx)
	a, ok, err := ClaudeCodePromptCache(detached, "muse", nil, nil)
	b, _, _ := ClaudeCodePromptCache(reqCtx, "muse", nil, nil)
	if err != nil || !ok || a.ID != b.ID {
		t.Fatal("own metadata lost across handler context")
	}
}
