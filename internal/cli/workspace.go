package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/KingJammin/blazn/internal/auth"
	"github.com/KingJammin/blazn/internal/client"
	workspacepkg "github.com/KingJammin/blazn/internal/workspace"
)

type workspaceCommands interface {
	Create(context.Context, string, string, string) (client.WorkspaceEnvelope, error)
	List(context.Context) (client.WorkspaceList, error)
	Get(context.Context, string) (client.WorkspaceEnvelope, error)
	Edit(context.Context, string, string, int, string) (client.WorkspaceEnvelope, error)
	Use(context.Context, string) (client.WorkspaceEnvelope, error)
	Invite(context.Context, string, client.Role, int, string) (client.InvitationCreated, error)
	Invitations(context.Context, string) (client.InvitationList, error)
	RevokeInvitation(context.Context, string, string, int, string) (client.MutationResult, error)
	Join(context.Context, string, string) (client.WorkspaceEnvelope, error)
	Members(context.Context, string) (client.MembershipList, error)
	SetRole(context.Context, string, string, client.Role, int, string) (client.Membership, error)
	RemoveMember(context.Context, string, string, int, string) (client.MutationResult, error)
	Leave(context.Context, string, string) (client.MutationResult, error)
	Events(context.Context, string, string) (*client.WorkspaceEventStream, error)
}

type workspaceOptions struct{ workspace, requestID string }

func (a *App) runWorkspace(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "workspace")
	}
	command := args[0]
	opts, rest, err := parseWorkspaceOptions(args[1:])
	if err != nil {
		return a.writeError(format, ExitUsage, "usage", err.Error())
	}
	if command == "join" && (len(rest) != 1 || rest[0] != "--invite-stdin" || opts.workspace != "") {
		return a.workspaceUsage(format, errors.New("join requires --invite-stdin; invitation tokens are never accepted as arguments"))
	}
	commands, err := a.workspace()
	if err != nil {
		return a.writeWorkspaceError(format, err)
	}
	ctx := context.Background()
	switch command {
	case "create":
		if opts.workspace != "" {
			return a.workspaceUsage(format, errors.New("create does not accept --workspace"))
		}
		name, flags, err := positionalAndFlags(rest, 1, map[string]bool{"slug": true})
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		result, err := commands.Create(ctx, name[0], flags["slug"], opts.requestID)
		return a.writeWorkspaceEnvelope(format, result, err, "created")
	case "list":
		if len(rest) != 0 || opts.workspace != "" || opts.requestID != "" {
			return a.workspaceUsage(format, errors.New("workspace list accepts no arguments"))
		}
		result, err := commands.List(ctx)
		return a.writeWorkspaceList(format, result, err)
	case "get":
		if opts.requestID != "" {
			return a.workspaceUsage(format, errors.New("get does not accept --request-id"))
		}
		value, err := optionalWorkspace(rest, opts.workspace)
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		result, err := commands.Get(ctx, value)
		return a.writeWorkspaceEnvelope(format, result, err, "found")
	case "edit":
		if opts.workspace != "" {
			return a.workspaceUsage(format, errors.New("edit takes WORKSPACE positionally"))
		}
		pos, flags, err := positionalAndFlags(rest, 1, map[string]bool{"name": true, "expected-version": true})
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		version, err := positiveVersion(flags["expected-version"])
		if err != nil || flags["name"] == "" {
			return a.workspaceUsage(format, errors.New("edit requires --name and --expected-version"))
		}
		result, err := commands.Edit(ctx, pos[0], flags["name"], version, opts.requestID)
		return a.writeWorkspaceEnvelope(format, result, err, "updated")
	case "use":
		if len(rest) != 1 || opts.workspace != "" || opts.requestID != "" {
			return a.workspaceUsage(format, errors.New("workspace use requires WORKSPACE"))
		}
		result, err := commands.Use(ctx, rest[0])
		return a.writeWorkspaceEnvelope(format, result, err, "selected")
	case "invite":
		pos, flags, err := positionalAndFlags(rest, -1, map[string]bool{"role": true, "expires-in": true})
		if err != nil || len(pos) > 1 {
			return a.workspaceUsage(format, errors.New("invite accepts optional WORKSPACE and requires --role"))
		}
		value, err := combineWorkspace(pos, opts.workspace)
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		role, err := workspacepkg.ParseRole(flags["role"])
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		expires := 86400
		if flags["expires-in"] != "" {
			expires, err = workspacepkg.ParseExpiresIn(flags["expires-in"])
			if err != nil {
				return a.workspaceUsage(format, err)
			}
		}
		result, err := commands.Invite(ctx, value, role, expires, opts.requestID)
		return a.writeInvitationCreated(format, result, err)
	case "invitations":
		value, err := optionalWorkspace(rest, opts.workspace)
		if err != nil || opts.requestID != "" {
			return a.workspaceUsage(format, errors.New("invitations accepts optional WORKSPACE"))
		}
		result, err := commands.Invitations(ctx, value)
		return a.writeInvitationList(format, result, err)
	case "revoke-invite":
		pos, flags, err := positionalAndFlags(rest, 1, map[string]bool{"expected-version": true})
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		version, err := positiveVersion(flags["expected-version"])
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		result, err := commands.RevokeInvitation(ctx, opts.workspace, pos[0], version, opts.requestID)
		return a.writeMutation(format, result, err)
	case "join":
		if a.stdinTTY() {
			return a.workspaceUsage(format, errors.New("--invite-stdin requires piped stdin so the token is not echoed"))
		}
		token, err := readInviteToken(a.stdin)
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		result, err := commands.Join(ctx, token, opts.requestID)
		return a.writeWorkspaceEnvelope(format, result, err, "joined")
	case "members":
		value, err := optionalWorkspace(rest, opts.workspace)
		if err != nil || opts.requestID != "" {
			return a.workspaceUsage(format, errors.New("members accepts optional WORKSPACE"))
		}
		result, err := commands.Members(ctx, value)
		return a.writeMembershipList(format, result, err)
	case "set-role":
		pos, flags, err := positionalAndFlags(rest, 1, map[string]bool{"role": true, "expected-version": true})
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		role, err := workspacepkg.ParseRole(flags["role"])
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		version, err := positiveVersion(flags["expected-version"])
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		result, err := commands.SetRole(ctx, opts.workspace, pos[0], role, version, opts.requestID)
		return a.writeMembership(format, result, err)
	case "remove-member":
		pos, flags, err := positionalAndFlags(rest, 1, map[string]bool{"expected-version": true})
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		version, err := positiveVersion(flags["expected-version"])
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		result, err := commands.RemoveMember(ctx, opts.workspace, pos[0], version, opts.requestID)
		return a.writeMutation(format, result, err)
	case "leave":
		if len(rest) != 0 {
			return a.workspaceUsage(format, errors.New("leave accepts no positional arguments"))
		}
		result, err := commands.Leave(ctx, opts.workspace, opts.requestID)
		return a.writeMutation(format, result, err)
	case "watch":
		if format == OutputJSON || opts.requestID != "" {
			return a.workspaceUsage(format, errors.New("workspace watch uses the SSE stream format and does not accept JSON or request IDs"))
		}
		pos, flags, err := positionalAndFlags(rest, -1, map[string]bool{"cursor": true})
		if err != nil || len(pos) > 1 {
			return a.workspaceUsage(format, errors.New("watch accepts optional WORKSPACE and --cursor"))
		}
		value, err := combineWorkspace(pos, opts.workspace)
		if err != nil {
			return a.workspaceUsage(format, err)
		}
		stream, err := commands.Events(ctx, value, flags["cursor"])
		if err != nil {
			return a.writeWorkspaceError(format, err)
		}
		defer stream.Body.Close()
		if _, err := io.Copy(a.stdout, stream.Body); err != nil {
			return a.writeWorkspaceError(format, err)
		}
		return ExitSuccess
	default:
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown workspace command %q", command))
	}
}

func parseWorkspaceOptions(args []string) (workspaceOptions, []string, error) {
	var opts workspaceOptions
	out := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var target *string
		switch {
		case arg == "--workspace":
			target = &opts.workspace
		case strings.HasPrefix(arg, "--workspace="):
			opts.workspace = strings.TrimPrefix(arg, "--workspace=")
			continue
		case arg == "--request-id":
			target = &opts.requestID
		case strings.HasPrefix(arg, "--request-id="):
			opts.requestID = strings.TrimPrefix(arg, "--request-id=")
			continue
		default:
			out = append(out, arg)
			continue
		}
		if i+1 >= len(args) {
			return opts, nil, fmt.Errorf("%s requires a value", arg)
		}
		i++
		*target = args[i]
	}
	if opts.workspace == "" {
		for _, arg := range args {
			if arg == "--workspace" || strings.HasPrefix(arg, "--workspace=") {
				return opts, nil, errors.New("--workspace requires a non-empty value")
			}
		}
	}
	if opts.requestID != "" && (len(opts.requestID) < 8 || len(opts.requestID) > 128) {
		return opts, nil, errors.New("--request-id must contain between 8 and 128 characters")
	}
	return opts, out, nil
}

func positionalAndFlags(args []string, count int, allowed map[string]bool) ([]string, map[string]string, error) {
	pos := []string{}
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			pos = append(pos, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		value := ""
		if split := strings.Index(name, "="); split >= 0 {
			value = name[split+1:]
			name = name[:split]
		} else {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		if !allowed[name] || value == "" {
			return nil, nil, fmt.Errorf("unknown or empty flag --%s", name)
		}
		flags[name] = value
	}
	if count >= 0 && len(pos) != count {
		return nil, nil, fmt.Errorf("expected %d positional argument(s)", count)
	}
	return pos, flags, nil
}

func optionalWorkspace(args []string, override string) (string, error) {
	if len(args) > 1 || (len(args) == 1 && override != "") {
		return "", errors.New("workspace may be supplied once, positionally or with --workspace")
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return override, nil
}
func combineWorkspace(pos []string, override string) (string, error) {
	if len(pos) == 1 && override != "" {
		return "", errors.New("workspace may be supplied once")
	}
	if len(pos) == 1 {
		return pos[0], nil
	}
	return override, nil
}
func positiveVersion(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return 0, errors.New("--expected-version must be at least 1")
	}
	return number, nil
}
func readInviteToken(reader io.Reader) (string, error) {
	input, err := io.ReadAll(io.LimitReader(reader, 514))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(input))
	if len(token) < 32 || len(token) > 512 {
		return "", errors.New("invitation token must contain between 32 and 512 characters")
	}
	scanner := bufio.NewScanner(strings.NewReader(string(input)))
	lines := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			lines++
		}
	}
	if lines != 1 {
		return "", errors.New("stdin must contain exactly one invitation token")
	}
	return token, nil
}

func (a *App) workspaceUsage(format OutputFormat, err error) int {
	return a.writeError(format, ExitUsage, "usage", err.Error())
}
func (a *App) writeWorkspaceError(format OutputFormat, err error) int {
	if errors.Is(err, workspacepkg.ErrNoContext) {
		return a.writeError(format, ExitUsage, "workspace_context_required", "select a workspace or use --workspace")
	}
	if errors.Is(err, auth.ErrNotFound) {
		return a.writeError(format, ExitUnavailable, "not_authenticated", "run 'blazn auth login'")
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.Body.Code
		if code == "" {
			code = "api_error"
		}
		exit := ExitFailure
		if code == "access_expired" || code == "session_revoked" || code == "device_revoked" || code == "unauthorized" {
			exit = ExitUnavailable
		}
		return a.writeError(format, exit, code, apiErr.Error())
	}
	return a.writeError(format, ExitUnavailable, "workspace_unavailable", err.Error())
}
func (a *App) writeWorkspaceEnvelope(f OutputFormat, r client.WorkspaceEnvelope, e error, status string) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	fmt.Fprintf(a.stdout, "%s workspace %s (%s)\n", titleWord(status), r.Workspace.Name, r.Workspace.ID)
	return ExitSuccess
}
func (a *App) writeWorkspaceList(f OutputFormat, r client.WorkspaceList, e error) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if r.Items == nil {
		r.Items = []client.Workspace{}
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	for _, w := range r.Items {
		fmt.Fprintf(a.stdout, "%-36s %-24s %-14s %s\n", w.ID, w.Slug, w.CurrentUserRole, w.Name)
	}
	return ExitSuccess
}
func (a *App) writeInvitationCreated(f OutputFormat, r client.InvitationCreated, e error) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	fmt.Fprintf(a.stdout, "Invitation %s (%s)\nToken: %s\n", r.Invitation.ID, r.Invitation.Role, r.InviteToken)
	return ExitSuccess
}
func (a *App) writeInvitationList(f OutputFormat, r client.InvitationList, e error) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if r.Items == nil {
		r.Items = []client.Invitation{}
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	for _, v := range r.Items {
		fmt.Fprintf(a.stdout, "%-36s %-14s %-10s %s\n", v.ID, v.Role, v.Status, v.ExpiresAt)
	}
	return ExitSuccess
}
func (a *App) writeMembershipList(f OutputFormat, r client.MembershipList, e error) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if r.Items == nil {
		r.Items = []client.Membership{}
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	for _, m := range r.Items {
		fmt.Fprintf(a.stdout, "%-36s %-14s %-10s %s\n", m.User.ID, m.Role, m.Status, m.User.DisplayName)
	}
	return ExitSuccess
}
func (a *App) writeMembership(f OutputFormat, r client.Membership, e error) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	fmt.Fprintf(a.stdout, "Updated %s to %s (version %d)\n", r.User.ID, r.Role, r.Version)
	return ExitSuccess
}
func (a *App) writeMutation(f OutputFormat, r client.MutationResult, e error) int {
	if e != nil {
		return a.writeWorkspaceError(f, e)
	}
	if f == OutputJSON {
		return a.writeJSON(r)
	}
	fmt.Fprintf(a.stdout, "%s workspace %s (version %d)\n", titleWord(r.Status), r.WorkspaceID, r.Version)
	return ExitSuccess
}

func titleWord(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
