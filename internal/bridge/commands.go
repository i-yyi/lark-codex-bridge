package bridge

import "strings"

type CommandName string

const (
	CommandNone    CommandName = ""
	CommandUnknown CommandName = "unknown"
	CommandHelp    CommandName = "help"
	CommandStatus  CommandName = "status"
	CommandCreate  CommandName = "create"
	CommandStop    CommandName = "stop"
	CommandApprove CommandName = "approve"
	CommandDeny    CommandName = "deny"
	CommandCancel  CommandName = "cancel"
)

type Command struct {
	Name      CommandName
	Arg       string
	Raw       string
	IsCommand bool
}

func ParseCommand(text string) Command {
	raw := strings.TrimSpace(text)
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return Command{Name: CommandNone, Raw: text}
	}

	token, arg, _ := strings.Cut(raw, " ")
	name := strings.TrimPrefix(strings.ToLower(token), "/")
	cmd := Command{
		Name:      CommandUnknown,
		Arg:       strings.TrimSpace(arg),
		Raw:       raw,
		IsCommand: true,
	}

	switch name {
	case "help":
		cmd.Name = CommandHelp
	case "status":
		cmd.Name = CommandStatus
	case "create":
		cmd.Name = CommandCreate
	case "stop":
		cmd.Name = CommandStop
	case "approve":
		cmd.Name = CommandApprove
	case "deny":
		cmd.Name = CommandDeny
	case "cancel":
		cmd.Name = CommandCancel
	}

	return cmd
}
