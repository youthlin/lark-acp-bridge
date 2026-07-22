package arg_test

import (
	"log/slog"
	"os"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/arg"
)

type User struct {
	ID   uint
	Name string
}

func TestJSON(t *testing.T) {
	var u = User{ID: 1, Name: "Bob"}
	slog.Info("user test", "user", arg.JSON(u))
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Info("user test", "user", arg.JSON(u))
	t.Logf("user=%s, s2b: %v", arg.JSON(u), arg.S2b(`hello`))
}
