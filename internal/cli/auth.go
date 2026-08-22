package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
)

func (a *App) runAuth(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "auth")
	}
	commands, err := a.auth()
	if err != nil {
		return a.writeError(format, ExitUnavailable, "credential_store_unavailable", err.Error())
	}
	ctx := context.Background()
	switch args[0] {
	case "login":
		return a.runAuthLogin(ctx, commands, format, args[1:])
	case "status":
		if len(args) != 1 {
			return a.writeError(format, ExitUsage, "usage", "auth status does not accept arguments")
		}
		return a.writeAuthStatus(ctx, commands, format)
	case "logout":
		if len(args) != 1 {
			return a.writeError(format, ExitUsage, "usage", "auth logout does not accept arguments")
		}
		return a.writeAuthLogout(ctx, commands, format)
	case "devices":
		if len(args) != 1 {
			return a.writeError(format, ExitUsage, "usage", "auth devices does not accept arguments")
		}
		return a.writeAuthDevices(ctx, commands, format)
	case "revoke-device":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return a.writeError(format, ExitUsage, "usage", "auth revoke-device requires DEVICE_ID")
		}
		return a.writeAuthRevoke(ctx, commands, format, args[1])
	default:
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown auth command %q", args[0]))
	}
}

func (a *App) runAuthLogin(ctx context.Context, commands authCommands, format OutputFormat, args []string) int {
	noBrowser := false
	for _, arg := range args {
		if arg == "--no-browser" {
			noBrowser = true
			continue
		}
		return a.writeError(format, ExitUsage, "usage", fmt.Sprintf("unknown auth login option %q", arg))
	}
	if noBrowser && format == OutputJSON {
		return a.writeError(format, ExitUsage, "usage", "auth login --no-browser requires human output so the one-time code is visible")
	}
	start, deviceCode, interval, err := commands.BeginLogin(ctx)
	if err != nil {
		return a.writeAuthError(format, err)
	}
	if format == OutputHuman {
		fmt.Fprintf(a.stdout, "Open %s and enter code %s\n", start.VerificationURI, start.UserCode)
	}
	if !noBrowser {
		uri := start.VerificationURIComplete
		if uri == "" {
			uri = start.VerificationURI
		}
		if err := a.openBrowser(uri); err != nil && format == OutputHuman {
			fmt.Fprintf(a.stderr, "blazn: could not open browser; continue at the URL above: %v\n", err)
		}
	}
	loginCtx, cancel := context.WithTimeout(ctx, time.Duration(start.ExpiresIn)*time.Second)
	defer cancel()
	result, err := commands.CompleteLogin(loginCtx, deviceCode, interval)
	if err != nil {
		return a.writeAuthError(format, err)
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	fmt.Fprintf(a.stdout, "Authenticated as %s on device %s.\n", result.User.DisplayName, result.DeviceID)
	return ExitSuccess
}

func (a *App) writeAuthStatus(ctx context.Context, commands authCommands, format OutputFormat) int {
	result, err := commands.Status(ctx)
	if err != nil {
		return a.writeAuthError(format, err)
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	if !result.Authenticated {
		fmt.Fprintf(a.stdout, "Not authenticated. Credential store: %s.\n", result.Store)
		return ExitSuccess
	}
	fmt.Fprintf(a.stdout, "Authenticated as %s (%s).\n", result.User.DisplayName, result.User.ID)
	fmt.Fprintf(a.stdout, "Device: %s (%s)\n", result.Device.Name, result.Device.ID)
	fmt.Fprintf(a.stdout, "Credential store: %s\n", result.Store)
	return ExitSuccess
}

func (a *App) writeAuthLogout(ctx context.Context, commands authCommands, format OutputFormat) int {
	result, err := commands.Logout(ctx)
	if err != nil {
		return a.writeAuthError(format, err)
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	fmt.Fprintln(a.stdout, "Logged out and revoked this device session.")
	return ExitSuccess
}

type devicesOutput struct {
	Items []client.Device `json:"items"`
}

func (a *App) writeAuthDevices(ctx context.Context, commands authCommands, format OutputFormat) int {
	devices, err := commands.Devices(ctx)
	if err != nil {
		return a.writeAuthError(format, err)
	}
	if devices == nil {
		devices = []client.Device{}
	}
	if format == OutputJSON {
		return a.writeJSON(devicesOutput{Items: devices})
	}
	if len(devices) == 0 {
		fmt.Fprintln(a.stdout, "No devices.")
		return ExitSuccess
	}
	fmt.Fprintf(a.stdout, "%-24s %-20s %-10s %s\n", "ID", "NAME", "PLATFORM", "STATUS")
	for _, device := range devices {
		fmt.Fprintf(a.stdout, "%-24s %-20s %-10s %s\n", device.ID, device.Name, device.Platform, device.Status)
	}
	return ExitSuccess
}

type revokeDeviceOutput struct {
	Status   string `json:"status"`
	DeviceID string `json:"deviceId"`
}

func (a *App) writeAuthRevoke(ctx context.Context, commands authCommands, format OutputFormat, deviceID string) int {
	if err := commands.RevokeDevice(ctx, deviceID); err != nil {
		return a.writeAuthError(format, err)
	}
	result := revokeDeviceOutput{Status: "revoked", DeviceID: deviceID}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	fmt.Fprintf(a.stdout, "Revoked device %s.\n", deviceID)
	return ExitSuccess
}

func (a *App) writeAuthError(format OutputFormat, err error) int {
	if errors.Is(err, auth.ErrNotFound) {
		return a.writeError(format, ExitFailure, "not_authenticated", "not authenticated; run 'blazn auth login'")
	}
	var apiError *client.APIError
	if errors.As(err, &apiError) {
		code := apiError.Body.Code
		if code == "" {
			code = "api_error"
		}
		return a.writeError(format, ExitFailure, code, apiError.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return a.writeError(format, ExitFailure, "authorization_expired", "device authorization expired")
	}
	return a.writeError(format, ExitUnavailable, "api_unavailable", err.Error())
}
