package bot

import "testing"

func TestCommandContext_SetAndGet(t *testing.T) {
	ctx := NewCommandContext()

	if ctx.Network() != "" {
		t.Error("expected empty network initially")
	}
	if ctx.Channel() != "" {
		t.Error("expected empty channel initially")
	}

	ctx.Set("efnet", "#test")
	if ctx.Network() != "efnet" {
		t.Errorf("expected network 'efnet', got %q", ctx.Network())
	}
	if ctx.Channel() != "#test" {
		t.Errorf("expected channel '#test', got %q", ctx.Channel())
	}
}

func TestCommandContext_Clear(t *testing.T) {
	ctx := NewCommandContext()
	ctx.Set("efnet", "#test")
	ctx.Clear()

	if ctx.Network() != "" {
		t.Error("expected empty network after clear")
	}
	if ctx.Channel() != "" {
		t.Error("expected empty channel after clear")
	}
}

func TestCommandContext_Resolve(t *testing.T) {
	ctx := NewCommandContext()
	ctx.Set("efnet", "#default")

	// Explicit values override context
	if got := ctx.ResolveNetwork("undernet"); got != "undernet" {
		t.Errorf("expected 'undernet', got %q", got)
	}
	if got := ctx.ResolveChannel("#other"); got != "#other" {
		t.Errorf("expected '#other', got %q", got)
	}

	// Empty explicit falls back to context
	if got := ctx.ResolveNetwork(""); got != "efnet" {
		t.Errorf("expected 'efnet', got %q", got)
	}
	if got := ctx.ResolveChannel(""); got != "#default" {
		t.Errorf("expected '#default', got %q", got)
	}
}

func TestCommandContext_String(t *testing.T) {
	ctx := NewCommandContext()
	if ctx.String() != "no context set" {
		t.Errorf("unexpected string: %q", ctx.String())
	}

	ctx.Set("efnet", "")
	if ctx.String() != "network: efnet" {
		t.Errorf("unexpected string: %q", ctx.String())
	}

	ctx.Set("efnet", "#test")
	if ctx.String() != "network: efnet, channel: #test" {
		t.Errorf("unexpected string: %q", ctx.String())
	}
}
