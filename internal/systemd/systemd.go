package systemd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func ActiveState(ctx context.Context, unit string) (string, error) {
	output, err := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	state := strings.TrimSpace(string(output))
	if state != "" {
		return state, nil
	}
	if err != nil {
		return "", fmt.Errorf("check systemd unit %s: %w", unit, err)
	}
	return "unknown", nil
}
