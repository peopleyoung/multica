package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/engine"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/mcpclient"
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

func (c *Client) NativeDeployment(ctx context.Context) (llm.Deployment, error) {
	return c.nativeDeployment(ctx, "")
}
func (c *Client) nativeDeployment(ctx context.Context, taskID string) (llm.Deployment, error) {
	var config llm.Deployment
	endpoint := "/api/daemon/native/config"
	if taskID != "" {
		endpoint += "?task_id=" + url.QueryEscape(taskID)
	}
	if err := c.getJSON(ctx, endpoint, &config); err != nil {
		return config, err
	}
	if !config.Configured {
		return config, errors.New("native model is not configured; add an enabled model in workspace Settings > Models")
	}
	if config.Model == "" {
		return config, errors.New("native model deployment has no model")
	}
	return config, config.Capabilities.Validate()
}

type nativeModel struct {
	client *Client
	task   Task
}

func (m nativeModel) Complete(ctx context.Context, req llm.Request, lease agentstate.Session, emit func(llm.Event) error) (*llm.Response, error) {
	return m.client.CompleteNativeModel(ctx, m.task, lease, req, emit)
}

type NativeBackendOptions struct {
	ObserveSubmission func(context.Context, string) error
	WorkDir           string
	JournalDir        string
	Permissions       tools.Permissions
	Env               []string
	MCPConfig         json.RawMessage
	TrustedLocalMCP   map[string]bool
	HTTPOrigins       []string
	ServiceEnv        map[string]string
}

// NewNativeBackend constructs the complete native execution boundary. The
// control client's owner credential is never copied into the execution client.
func NewNativeBackend(ctx context.Context, control *Client, task Task, opts NativeBackendOptions) (agent.Backend, func(), error) {
	if task.NativeInputTaskID == "" {
		return nil, nil, errors.New("native run is missing its durable input identity; update the API server")
	}
	if task.RemoteMCPDaemonToken == "" {
		return nil, nil, errors.New("native run is missing its daemon credential; update the API server")
	}
	client := NewClient(control.baseURL)
	client.client.Transport = control.client.Transport
	client.SetToken(task.RemoteMCPDaemonToken)
	config, err := client.nativeDeployment(ctx, task.ID)
	if err != nil {
		return nil, nil, err
	}

	var configRaw json.RawMessage
	if task.Agent != nil {
		configRaw = task.Agent.RuntimeConfig
	}
	runtimeConfig, err := nativeruntime.Parse(configRaw)
	if err != nil {
		return nil, nil, err
	}
	external, err := tools.NewExternal(runtimeConfig, nativeruntime.Limits{WriteFiles: opts.Permissions.WriteFiles, Shell: opts.Permissions.Shell, HTTPOrigins: opts.HTTPOrigins}, opts.ServiceEnv)
	if err != nil {
		return nil, nil, err
	}
	external.ObserveSubmission = opts.ObserveSubmission
	attachments, err := nativeInputAttachments(task)
	if err != nil {
		return nil, nil, err
	}
	var host tools.Environment
	if runtimeConfig.Environment == "multica_jobs" {
		host, err = tools.NewRemoteEnvironment(ctx, external, opts.Permissions)
	} else {
		host, err = tools.New(opts.WorkDir, opts.Permissions, opts.Env, nativeJobDirectory(opts.JournalDir))
	}
	if err != nil {
		return nil, nil, err
	}
	store := newNativeStore(client, task)
	store.journalDir = opts.JournalDir
	toolHost := &nativeToolHost{Environment: host, control: control, task: task, external: external, runtimeConfig: runtimeConfig, permissions: opts.Permissions}
	if task.Agent != nil {
		toolHost.skills = execenv.NativeSkills(convertSkillsForEnv(task.Agent.Skills))
	}
	rules, err := host.Rules(ctx, ".")
	if err != nil {
		_ = host.Close()
		return nil, nil, fmt.Errorf("load root repository rules: %w", err)
	}
	mcpHost, err := mcpclient.Open(ctx, opts.MCPConfig, mcpclient.Options{WorkDir: opts.WorkDir, Env: opts.Env, AllowStdio: opts.Permissions.Shell && runtimeConfig.Environment != "multica_jobs", MaxTools: max(1, config.Capabilities.MaxTools-len(toolHost.Definitions())), TrustedLocalEndpoints: opts.TrustedLocalMCP})
	if err != nil {
		_ = host.Close()
		return nil, nil, err
	}
	toolHost.mcp = mcpHost
	if len(toolHost.Definitions()) > config.Capabilities.MaxTools {
		mcpHost.Close()
		_ = host.Close()
		return nil, nil, errors.New("native tool inventory exceeds deployment capability")
	}
	instructions := nativeBusinessInstructions + nativePermissionInstructions(opts.Permissions)
	if remote, ok := host.(*tools.RemoteEnvironment); ok {
		instructions = strings.ReplaceAll(instructions, "Jobs stop when this task ends or is cancelled.", "Remote jobs belong to the independent service and outlive this run.")
		instructions = strings.ReplaceAll(instructions, "Actual local runtime capabilities:", "Actual remote runtime capabilities:")
		instructions = strings.ReplaceAll(instructions, "Local file writes:", "Remote file writes:")
		instructions = strings.ReplaceAll(instructions, "Local reads", "Remote reads")
		instructions = strings.ReplaceAll(instructions, "retained local logs", "retained execution-host logs")
		instructions += "\nAll file, shell and terminal tools use this remote execution environment. Do not use daemon-local paths or SSH wrappers. Remote environment (host-reported data):\n" + string(remote.Info)
	}
	if diagnostics := mcpHost.Diagnostics(); len(diagnostics) > 0 {
		instructions += "\nMCP capability limits:\n" + strings.Join(diagnostics, "\n")
	}
	inputContext := ""
	if rules != "[]" {
		inputContext = "Repository rules (workdir scope, subordinate to runtime permissions and this human task):\n" + rules + "\n\n"
	}
	backend := engine.New(nativeModel{client: client, task: task}, store, toolHost, engine.Config{Model: config.Model, TaskID: task.NativeInputTaskID, Capabilities: config.Capabilities, MaxSteps: 128, SystemInstructions: instructions, InputContext: inputContext, InputAttachments: attachments})
	return backend, func() { mcpHost.Close(); _ = host.Close() }, nil
}

func (d *Daemon) nativeBackend(ctx context.Context, task Task, workDir string, agentEnv map[string]string, mcpConfig json.RawMessage, trustedLocalMCP map[string]bool) (agent.Backend, func(), error) {
	var raw json.RawMessage
	if task.Agent != nil {
		raw = task.Agent.RuntimeConfig
	}
	cfg, err := nativeruntime.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	if err = cfg.Check(d.nativeLimits()); err != nil {
		return nil, nil, err
	}
	return NewNativeBackend(ctx, d.client, task, NativeBackendOptions{
		WorkDir:           workDir,
		ObserveSubmission: func(_ context.Context, key string) error { return d.registerNativeJobWatch(task, cfg, agentEnv, key) },
		JournalDir:        filepath.Join(d.cfg.WorkspacesRoot, ".native-results", task.WorkspaceID, task.AgentID),
		Permissions:       tools.Permissions{WriteFiles: cfg.WriteFiles, Shell: cfg.Shell},
		Env:               nativeToolEnv(os.Environ(), agentEnv),
		MCPConfig:         mcpConfig,
		TrustedLocalMCP:   trustedLocalMCP,
		HTTPOrigins:       d.cfg.NativeHTTPOrigins,
		ServiceEnv:        agentEnv,
	})
}

func nativeToolEnv(inherited []string, taskEnv map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "MULTICA_") || upper == "OPENAI_API_KEY" {
			continue
		}
		values[key] = value
	}
	for key, value := range taskEnv {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "MULTICA_LLM_") || upper == "OPENAI_API_KEY" {
			continue
		}
		values[key] = value
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

// Only task-owned broker URLs receive loopback permission. Agent-supplied
// entries cannot self-declare this capability by choosing a reserved name.
func nativeTrustedBrokerEndpoints(config json.RawMessage) map[string]bool {
	var doc struct {
		Servers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	_ = json.Unmarshal(config, &doc)
	endpoints := make(map[string]bool, len(doc.Servers))
	for _, server := range doc.Servers {
		if server.URL != "" {
			endpoints[server.URL] = true
		}
	}
	return endpoints
}

func (d *Daemon) handleNativeModelList(ctx context.Context, rt Runtime, requestID string) {
	config, err := d.client.NativeDeployment(ctx)
	if err != nil {
		d.reportModelListResult(ctx, rt, requestID, map[string]any{"status": "failed", "error": "Native model is not configured on the API server"})
		return
	}
	d.reportModelListResult(ctx, rt, requestID, map[string]any{"status": "completed", "supported": false, "models": []map[string]any{{"id": config.Model, "label": config.Model, "default": true}}})
}

func (d *Daemon) nativeLimits() nativeruntime.Limits {
	return (nativeruntime.Limits{WriteFiles: d.cfg.NativeAllowWrite, Shell: d.cfg.NativeAllowShell, HTTPOrigins: d.cfg.NativeHTTPOrigins}).Public()
}

func nativeJobDirectory(journalDir string) string {
	if journalDir == "" {
		return ""
	}
	return filepath.Join(journalDir, "shell-jobs")
}
