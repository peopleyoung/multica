package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type nativeManagementOperation struct {
	action, method, suffix string
	properties             map[string]json.RawMessage
	required               []string
}

type nativeManagementSpec struct {
	name, path, description string
	operations              []nativeManagementOperation
}

func managementOperation(action, method, suffix, properties string, required ...string) nativeManagementOperation {
	op := nativeManagementOperation{action: action, method: method, suffix: suffix, required: required}
	if err := json.Unmarshal([]byte(properties), &op.properties); err != nil {
		panic("invalid built-in management schema: " + err.Error())
	}
	return op
}

var nativeManagementSpecs = []nativeManagementSpec{
	{
		name: "multica_manage_project", path: "/api/projects",
		description: "Read, create or update a project, list its resources, or bind a repository/local directory. Uses existing workspace permissions and resource validation. No repository checkout or execution is started by configuration. Dates use YYYY-MM-DD; lead type and id must be supplied together.",
		operations: []nativeManagementOperation{
			managementOperation("get", http.MethodGet, "", `{}`),
			managementOperation("create", http.MethodPost, "", `{"title":{"type":"string","minLength":1,"maxLength":500},"description":{"type":"string","maxLength":32768},"status":{"type":"string"},"priority":{"type":"string"},"lead_type":{"type":"string","enum":["member","agent"]},"lead_id":{"type":"string"},"start_date":{"type":"string"},"due_date":{"type":"string"}}`, "title"),
			managementOperation("update", http.MethodPut, "", `{"title":{"type":"string","minLength":1,"maxLength":500},"description":{"type":["string","null"],"maxLength":32768},"status":{"type":"string"},"priority":{"type":"string"},"lead_type":{"type":["string","null"],"enum":["member","agent",null]},"lead_id":{"type":["string","null"]},"start_date":{"type":["string","null"]},"due_date":{"type":["string","null"]}}`),
			managementOperation("list_resources", http.MethodGet, "/resources", `{}`),
			managementOperation("add_resource", http.MethodPost, "/resources", `{"resource_type":{"type":"string","enum":["github_repo","local_directory"]},"resource_ref":{"type":"object","properties":{"url":{"type":"string"},"default_branch_hint":{"type":"string"},"ref":{"type":"string"},"daemon_id":{"type":"string"},"local_path":{"type":"string"},"execution_mode":{"type":"string","enum":["in_place","worktree"]}},"additionalProperties":false},"label":{"type":"string","maxLength":500}}`, "resource_type", "resource_ref"),
		},
	},
	{
		name: "multica_manage_agent", path: "/api/agents",
		description: "Read, create or update an agent, or add enabled skills by exact id. Creation uses this run's native runtime and the deployment model, with private visibility by default. No model, CLI, credentials, MCP or environment override. Preserve the existing preview/confirmation workflow before creating or materially reconfiguring an agent. This configures an agent; it does not delegate or execute work.",
		operations: []nativeManagementOperation{
			managementOperation("get", http.MethodGet, "", `{}`),
			managementOperation("create", http.MethodPost, "", `{"name":{"type":"string","minLength":1,"maxLength":200},"description":{"type":"string","maxLength":2000},"instructions":{"type":"string","maxLength":32768},"visibility":{"type":"string","enum":["private","workspace"]},"skill_ids":{"type":"array","items":{"type":"string"},"maxItems":32,"uniqueItems":true}}`, "name", "instructions"),
			managementOperation("update", http.MethodPut, "", `{"name":{"type":"string","minLength":1,"maxLength":200},"description":{"type":"string","maxLength":2000},"instructions":{"type":"string","maxLength":32768}}`),
			managementOperation("add_skills", http.MethodPost, "/skills/add", `{"skill_ids":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":32,"uniqueItems":true}}`, "skill_ids"),
		},
	},
	{
		name: "multica_manage_squad", path: "/api/squads",
		description: "Read, create or update a squad, list its members, or add a member. Existing leader invocation and creator/admin gates apply. Preserve the existing preview/confirmation workflow for squad configuration. This does not add delegation beyond existing assignment and comment triggers.",
		operations: []nativeManagementOperation{
			managementOperation("get", http.MethodGet, "", `{}`),
			managementOperation("create", http.MethodPost, "", `{"name":{"type":"string","minLength":1,"maxLength":200},"description":{"type":"string","maxLength":32768},"leader_id":{"type":"string"}}`, "name", "leader_id"),
			managementOperation("update", http.MethodPut, "", `{"name":{"type":"string","minLength":1,"maxLength":200},"description":{"type":"string","maxLength":32768},"leader_id":{"type":"string"}}`),
			managementOperation("list_members", http.MethodGet, "/members", `{}`),
			managementOperation("add_member", http.MethodPost, "/members", `{"member_type":{"type":"string","enum":["agent","member"]},"member_id":{"type":"string"}}`, "member_type", "member_id"),
		},
	},
	{
		name: "multica_manage_autopilot", path: "/api/autopilots",
		description: "Read, create or update an automation, or add a cron/webhook trigger. Existing originator, collaborator, quota and assignee gates apply. Preserve the preview/confirmation workflow before changing future execution. Webhook credentials are never returned; a human with access retrieves connection details from automation settings. No manual trigger or replay is provided.",
		operations: []nativeManagementOperation{
			managementOperation("get", http.MethodGet, "", `{}`),
			managementOperation("create", http.MethodPost, "", `{"title":{"type":"string","minLength":1,"maxLength":500},"description":{"type":"string","maxLength":32768},"project_id":{"type":"string"},"assignee_type":{"type":"string","enum":["agent","squad"]},"assignee_id":{"type":"string"},"execution_mode":{"type":"string","enum":["create_issue","run_only"]},"issue_title_template":{"type":"string","maxLength":500}}`, "title", "assignee_id", "execution_mode"),
			managementOperation("update", http.MethodPatch, "", `{"title":{"type":"string","minLength":1,"maxLength":500},"description":{"type":"string","maxLength":32768},"project_id":{"type":["string","null"]},"assignee_type":{"type":"string","enum":["agent","squad"]},"assignee_id":{"type":"string"},"status":{"type":"string","enum":["active","paused"]},"execution_mode":{"type":"string","enum":["create_issue","run_only"]},"issue_title_template":{"type":"string","maxLength":500}}`),
			managementOperation("add_trigger", http.MethodPost, "/triggers", `{"kind":{"type":"string","enum":["schedule","webhook"]},"cron_expression":{"type":"string","maxLength":200},"timezone":{"type":"string","maxLength":100},"label":{"type":"string","maxLength":500},"provider":{"type":"string","enum":["generic","github"]}}`, "kind"),
		},
	},
}

func nativeManagementDefinitions() []llm.Tool {
	var definitions []llm.Tool
	for _, spec := range nativeManagementSpecs {
		actions := make([]string, 0, len(spec.operations))
		var contract strings.Builder
		properties := map[string]json.RawMessage{}
		for _, op := range spec.operations {
			actions = append(actions, op.action)
			if len(op.required) > 0 {
				fmt.Fprintf(&contract, " %s requires values: %s.", op.action, strings.Join(op.required, ", "))
			}
			for key, property := range op.properties {
				// Update may permit null to explicitly clear optional fields.
				properties[key] = property
			}
		}
		schema, _ := json.Marshal(map[string]any{
			"type": "object", "properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": actions},
				"id":     map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "description": "Required except for create; exact resource id."},
				"values": map[string]any{"type": "object", "properties": properties, "additionalProperties": false, "description": "Fields for the chosen write action; omitted for reads. Create requires title/name and the resource's required configuration."},
			}, "required": []string{"action"}, "additionalProperties": false,
		})
		definitions = append(definitions, llm.Tool{Name: spec.name, Description: spec.description + contract.String(), Parameters: schema})
	}
	return definitions
}

func (h *nativeToolHost) management(ctx context.Context, call llm.ToolCall) (string, error) {
	var args struct {
		Action string                     `json:"action"`
		ID     string                     `json:"id"`
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := decodeNativeToolArguments(call.Arguments, &args); err != nil {
		return "", err
	}
	for _, spec := range nativeManagementSpecs {
		if spec.name != call.Name {
			continue
		}
		for _, op := range spec.operations {
			if op.action != args.Action {
				continue
			}
			if args.Action == "create" && args.ID != "" || args.Action != "create" && !nativeResourceID(args.ID) {
				return "", errors.New("create accepts no id; all other configuration actions require a resource id")
			}
			if op.method == http.MethodGet && args.Values != nil || op.method != http.MethodGet && len(args.Values) == 0 {
				return "", errors.New("reads accept no values; writes require nonempty values")
			}
			if op.method != http.MethodGet {
				if err := validateManagementValues(op, args.Values); err != nil {
					return "", err
				}
			}
			path := spec.path
			if args.ID != "" {
				path += "/" + url.PathEscape(args.ID)
			}
			if call.Name == "multica_manage_agent" && args.Action == "create" {
				if !nativeResourceID(h.task.RuntimeID) {
					return "", errors.New("agent creation requires this run's native runtime")
				}
				raw, _ := json.Marshal(h.task.RuntimeID)
				args.Values["runtime_id"] = raw
			}
			return h.request(ctx, op.method, path+op.suffix, args.Values)
		}
		return "", errors.New("unsupported configuration action")
	}
	return "", errors.New("unknown configuration tool")
}

func validateManagementValues(op nativeManagementOperation, values map[string]json.RawMessage) error {
	raw, _ := json.Marshal(map[string]any{"type": "object", "properties": op.properties, "required": op.required, "additionalProperties": false})
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return err
	}
	var decoded map[string]any
	raw, _ = json.Marshal(values)
	_ = json.Unmarshal(raw, &decoded)
	if resolved.Validate(decoded) != nil {
		return errors.New("configuration fields do not match the chosen action's schema")
	}
	for key, raw := range values {
		if key == "id" || strings.HasSuffix(key, "_id") && string(raw) != "null" {
			var id string
			if json.Unmarshal(raw, &id) != nil || !nativeResourceID(id) {
				return fmt.Errorf("invalid configuration identifier: %s", key)
			}
		}
	}
	if _, lead := values["lead_type"]; lead {
		if _, id := values["lead_id"]; !id {
			return errors.New("lead_type and lead_id must be supplied together")
		}
	} else if _, id := values["lead_id"]; id {
		return errors.New("lead_type and lead_id must be supplied together")
	}
	return nil
}
