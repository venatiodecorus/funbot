package controller

import (
	"fmt"
	"log/slog"
	"strings"
)

// CommandHandler is a function that handles a parsed command.
// It receives the command arguments and returns a response string.
type CommandHandler func(args []string) string

// CommandDispatcher parses and routes incoming IRC commands.
type CommandDispatcher struct {
	prefix   string
	ctx      *CommandContext
	handlers map[string]CommandHandler
	log      *slog.Logger
}

// NewCommandDispatcher creates a new dispatcher with the given command prefix.
func NewCommandDispatcher(prefix string, cmdCtx *CommandContext, log *slog.Logger) *CommandDispatcher {
	d := &CommandDispatcher{
		prefix:   prefix,
		ctx:      cmdCtx,
		handlers: make(map[string]CommandHandler),
		log:      log,
	}
	d.registerBuiltinCommands()
	return d
}

// Register adds a command handler. The name should not include the prefix.
func (d *CommandDispatcher) Register(name string, handler CommandHandler) {
	d.handlers[strings.ToLower(name)] = handler
}

// Dispatch parses a raw message and executes the matching command handler.
// Returns the response string, or empty string if not a command.
func (d *CommandDispatcher) Dispatch(rawMessage string) string {
	rawMessage = strings.TrimSpace(rawMessage)
	if !strings.HasPrefix(rawMessage, d.prefix) {
		return ""
	}

	// Strip prefix
	rawMessage = rawMessage[len(d.prefix):]
	parts := strings.Fields(rawMessage)
	if len(parts) == 0 {
		return ""
	}

	cmdName := strings.ToLower(parts[0])
	args := parts[1:]

	handler, ok := d.handlers[cmdName]
	if !ok {
		return fmt.Sprintf("Unknown command: %s%s", d.prefix, cmdName)
	}

	d.log.Info("dispatching command", "command", cmdName, "args", args)
	return handler(args)
}

// registerBuiltinCommands registers the commands that are handled
// directly by the controller (context, help).
func (d *CommandDispatcher) registerBuiltinCommands() {
	d.Register("context", d.handleContext)
	d.Register("help", d.handleHelp)
}

// handleContext implements the !context command.
func (d *CommandDispatcher) handleContext(args []string) string {
	if len(args) == 0 {
		return fmt.Sprintf("Current context: %s", d.ctx.String())
	}

	if strings.ToLower(args[0]) == "clear" {
		d.ctx.Clear()
		return "Context cleared"
	}

	network := args[0]
	channel := ""
	if len(args) > 1 && strings.HasPrefix(args[1], "#") {
		channel = args[1]
	}

	d.ctx.Set(network, channel)
	return fmt.Sprintf("Context set: %s", d.ctx.String())
}

// handleHelp returns a list of available commands.
func (d *CommandDispatcher) handleHelp(args []string) string {
	cmds := make([]string, 0, len(d.handlers))
	for name := range d.handlers {
		cmds = append(cmds, d.prefix+name)
	}
	return "Available commands: " + strings.Join(cmds, ", ")
}

// IsChannel returns true if the string looks like an IRC channel name.
func IsChannel(s string) bool {
	return strings.HasPrefix(s, "#") || strings.HasPrefix(s, "&")
}
