package api

import "strings"

var adminCommands = map[string]bool{
	"/cancel": true,
	"/config": true,
	"/mcp":    true,
	"/run":    true,
}

func IsAdminCommand(cmd string) bool {
	if idx := strings.IndexByte(cmd, ' '); idx > 0 {
		cmd = cmd[:idx]
	}
	return adminCommands[cmd]
}
