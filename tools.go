package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/go-shiori/go-readability"
)

// --- Tool system ---

type AgentTool struct {
	name        string
	description string
	parameters  map[string]any
	execute     func(ctx context.Context, params map[string]any) (string, error)
}

// RequestContext holds per-request state for tools that need it (message, spawn).
type RequestContext struct {
	ChatID      string
	SessionKey  string
	MessageID   int  // Telegram message ID for reply-to
	MessageSent bool // set when message tool sends a reply, to suppress duplicate final response
}

type ToolRegistry struct {
	tools map[string]*AgentTool
}

func newToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]*AgentTool)}
}

func (r *ToolRegistry) register(t *AgentTool) {
	r.tools[t.name] = t
}

func (r *ToolRegistry) execute(ctx context.Context, name string, paramsJSON string) string {
	tool, ok := r.tools[name]
	if !ok {
		available := make([]string, 0, len(r.tools))
		for k := range r.tools {
			available = append(available, k)
		}
		return fmt.Sprintf("Error: unknown tool %q. Available tools: %s", name, strings.Join(available, ", "))
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		// Try basic JSON repair for malformed output from weaker models.
		repaired := repairJSON(paramsJSON)
		if err2 := json.Unmarshal([]byte(repaired), &params); err2 != nil {
			return fmt.Sprintf("Error: invalid parameters: %v\n\n[Analyze the error above and try a different approach.]", err)
		}
	}
	if errs := validateToolParams(tool.parameters, params); len(errs) > 0 {
		return fmt.Sprintf("Error: invalid parameters: %s\n\n[Analyze the error above and try a different approach.]", strings.Join(errs, "; "))
	}
	result, err := tool.execute(ctx, params)
	if err != nil {
		return fmt.Sprintf("Error: %v\n\n[Analyze the error above and try a different approach.]", err)
	}
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}
	return result
}

func (r *ToolRegistry) schemas() []ToolDef {
	defs := make([]ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, ToolDef{
			Type: "function",
			Function: FunctionDef{
				Name:        t.name,
				Description: t.description,
				Parameters:  t.parameters,
			},
		})
	}
	return defs
}

func (r *ToolRegistry) schemasExcluding(names ...string) []ToolDef {
	excl := make(map[string]bool, len(names))
	for _, n := range names {
		excl[n] = true
	}
	var defs []ToolDef
	for _, t := range r.tools {
		if !excl[t.name] {
			defs = append(defs, ToolDef{
				Type: "function",
				Function: FunctionDef{
					Name:        t.name,
					Description: t.description,
					Parameters:  t.parameters,
				},
			})
		}
	}
	return defs
}

func paramStr(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return v
}

func paramInt(params map[string]any, key string) int {
	switch v := params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func paramBool(params map[string]any, key string) bool {
	v, _ := params[key].(bool)
	return v
}

func validateToolParams(schema map[string]any, params map[string]any) []string {
	if schema == nil {
		return nil
	}
	// Tools in Cody should always declare object schemas.
	if typ, _ := schema["type"].(string); typ != "" && typ != "object" {
		return []string{fmt.Sprintf("parameter should be %s", typ)}
	}
	return validateSchema(params, schema, "")
}

func validateSchema(value any, schema map[string]any, path string) []string {
	typ, _ := schema["type"].(string)
	label := path
	if label == "" {
		label = "parameter"
	}

	if typ != "" {
		if !isSchemaType(value, typ) {
			return []string{fmt.Sprintf("%s should be %s", label, typ)}
		}
	}

	var errs []string
	if enumVals, ok := schema["enum"].([]any); ok && !enumContains(enumVals, value) {
		errs = append(errs, fmt.Sprintf("%s must be one of %v", label, enumVals))
	}

	switch typ {
	case "number", "integer":
		n, ok := numericValue(value, typ == "integer")
		if !ok {
			return []string{fmt.Sprintf("%s should be %s", label, typ)}
		}
		if min, ok := numericSchemaValue(schema["minimum"]); ok && n < min {
			errs = append(errs, fmt.Sprintf("%s must be >= %v", label, min))
		}
		if max, ok := numericSchemaValue(schema["maximum"]); ok && n > max {
			errs = append(errs, fmt.Sprintf("%s must be <= %v", label, max))
		}
	case "string":
		s, _ := value.(string)
		if min, ok := numericSchemaValue(schema["minLength"]); ok && float64(len(s)) < min {
			errs = append(errs, fmt.Sprintf("%s must be at least %d chars", label, int(min)))
		}
		if max, ok := numericSchemaValue(schema["maxLength"]); ok && float64(len(s)) > max {
			errs = append(errs, fmt.Sprintf("%s must be at most %d chars", label, int(max)))
		}
	case "object":
		obj, _ := value.(map[string]any)
		props := schemaMap(schema["properties"])
		for _, required := range schemaStringSlice(schema["required"]) {
			if _, ok := obj[required]; !ok {
				if path == "" {
					errs = append(errs, fmt.Sprintf("missing required %s", required))
				} else {
					errs = append(errs, fmt.Sprintf("missing required %s.%s", path, required))
				}
			}
		}
		for k, v := range obj {
			propSchema, ok := props[k]
			if !ok {
				continue
			}
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			errs = append(errs, validateSchema(v, propSchema, childPath)...)
		}
	case "array":
		arr, _ := value.([]any)
		itemSchema, ok := schema["items"].(map[string]any)
		if ok {
			for i, item := range arr {
				childPath := fmt.Sprintf("[%d]", i)
				if path != "" {
					childPath = fmt.Sprintf("%s[%d]", path, i)
				}
				errs = append(errs, validateSchema(item, itemSchema, childPath)...)
			}
		}
	}

	return errs
}

func isSchemaType(value any, typ string) bool {
	switch typ {
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		_, ok := numericValue(value, true)
		return ok
	case "number":
		_, ok := numericValue(value, false)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return true
	}
}

func numericValue(value any, requireInteger bool) (float64, bool) {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case int32:
		n = float64(v)
	case int16:
		n = float64(v)
	case int8:
		n = float64(v)
	case uint:
		n = float64(v)
	case uint64:
		n = float64(v)
	case uint32:
		n = float64(v)
	case uint16:
		n = float64(v)
	case uint8:
		n = float64(v)
	default:
		return 0, false
	}
	if requireInteger && math.Trunc(n) != n {
		return 0, false
	}
	return n, true
}

func numericSchemaValue(value any) (float64, bool) {
	return numericValue(value, false)
}

func schemaMap(value any) map[string]map[string]any {
	out := make(map[string]map[string]any)
	props, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for k, raw := range props {
		if s, ok := raw.(map[string]any); ok {
			out[k] = s
		}
	}
	return out
}

func schemaStringSlice(value any) []string {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func enumContains(enumVals []any, value any) bool {
	for _, candidate := range enumVals {
		if enumValueEqual(candidate, value) {
			return true
		}
	}
	return false
}

func enumValueEqual(a, b any) bool {
	na, oka := numericSchemaValue(a)
	nb, okb := numericSchemaValue(b)
	if oka && okb {
		return na == nb
	}
	return reflect.DeepEqual(a, b)
}

// resolvePath resolves relative paths against workspace and optionally rejects
// paths outside allowedDir when configured. Symlinks are evaluated to prevent
// traversal via symbolic links.
func resolvePath(workspace, path, allowedDir string) (string, error) {
	var resolved string
	if filepath.IsAbs(path) {
		resolved = filepath.Clean(path)
	} else {
		resolved = filepath.Clean(filepath.Join(workspace, path))
	}
	if allowedDir != "" {
		// Evaluate symlinks so a symlink inside the allowed dir cannot point outside it.
		real, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			// If the path doesn't exist yet (e.g. write_file creating a new file),
			// evaluate the parent directory instead.
			parent := filepath.Dir(resolved)
			realParent, perr := filepath.EvalSymlinks(parent)
			if perr != nil {
				return "", fmt.Errorf("cannot resolve path: %w", err)
			}
			real = filepath.Join(realParent, filepath.Base(resolved))
		}
		abs, err := filepath.Abs(real)
		if err != nil {
			return "", fmt.Errorf("resolve absolute path: %w", err)
		}
		dir, err := filepath.Abs(allowedDir)
		if err != nil {
			return "", fmt.Errorf("resolve allowed directory: %w", err)
		}
		if !strings.HasPrefix(abs, dir+string(filepath.Separator)) && abs != dir {
			return "", fmt.Errorf("access denied: path %q is outside allowed directory", path)
		}
	}
	return resolved, nil
}

// --- Built-in tools ---

func registerDefaultTools(reg *ToolRegistry, cfg *Config, workspace string, bus *MessageBus, reqCtx *RequestContext,
	cronSvc *CronService, subMgr *SubagentManager) {

	allowedDir := cfg.Tools.AllowedDir

	reg.register(makeReadFileTool(workspace, allowedDir))
	reg.register(makeWriteFileTool(workspace, allowedDir))
	reg.register(makeEditFileTool(workspace, allowedDir))
	reg.register(makeListDirTool(workspace, allowedDir))
	reg.register(makeExecTool(workspace, allowedDir, cfg.Tools.ExecTimeout, cfg.Tools.PathAppend))
	reg.register(makeWebSearchTool())
	reg.register(makeWebFetchTool())
	reg.register(makeMessageTool(bus, reqCtx))
	reg.register(makeCronTool(cronSvc, reqCtx))
	reg.register(makeSpawnTool(subMgr, reqCtx))
}

// --- read_file ---

func makeReadFileTool(workspace, allowedDir string) *AgentTool {
	return &AgentTool{
		name:        "read_file",
		description: "Read the contents of a file at the given path.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "The file path to read"},
			},
			"required": []string{"path"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			path, err := resolvePath(workspace, paramStr(params, "path"), allowedDir)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	}
}

// --- write_file ---

func makeWriteFileTool(workspace, allowedDir string) *AgentTool {
	return &AgentTool{
		name:        "write_file",
		description: "Create or overwrite a file with the given content.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path"},
				"content": map[string]any{"type": "string", "description": "File content to write"},
			},
			"required": []string{"path", "content"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			path, err := resolvePath(workspace, paramStr(params, "path"), allowedDir)
			if err != nil {
				return "", err
			}
			content := paramStr(params, "content")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Wrote %d bytes to %s", len(content), path), nil
		},
	}
}

// --- edit_file ---

func makeEditFileTool(workspace, allowedDir string) *AgentTool {
	return &AgentTool{
		name:        "edit_file",
		description: "Edit a file by replacing old_text with new_text. The old_text must exist exactly in the file.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":     map[string]any{"type": "string", "description": "File path"},
				"old_text": map[string]any{"type": "string", "description": "The exact text to find and replace"},
				"new_text": map[string]any{"type": "string", "description": "The text to replace with"},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			path, err := resolvePath(workspace, paramStr(params, "path"), allowedDir)
			if err != nil {
				return "", err
			}
			oldStr := paramStr(params, "old_text")
			newStr := paramStr(params, "new_text")

			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			content := string(data)
			count := strings.Count(content, oldStr)
			if count == 0 {
				// Find closest match to help the LLM correct its attempt.
				hint := findCloseMatch(content, oldStr)
				if hint != "" {
					return "", fmt.Errorf("old_text not found in file. %s", hint)
				}
				return "", fmt.Errorf("old_text not found in file")
			}
			if count > 1 {
				return "", fmt.Errorf("old_text found %d times, must be unique", count)
			}
			content = strings.Replace(content, oldStr, newStr, 1)
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Successfully edited %s", path), nil
		},
	}
}

// --- list_dir ---

func makeListDirTool(workspace, allowedDir string) *AgentTool {
	return &AgentTool{
		name:        "list_dir",
		description: "List the contents of a directory.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "The directory path to list"},
			},
			"required": []string{"path"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			path := paramStr(params, "path")
			if path == "" {
				path = workspace
			} else {
				resolved, err := resolvePath(workspace, path, allowedDir)
				if err != nil {
					return "", err
				}
				path = resolved
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return "", err
			}
			if len(entries) == 0 {
				return fmt.Sprintf("Directory %s is empty", path), nil
			}
			var items []string
			for _, e := range entries {
				prefix := "📄 "
				if e.IsDir() {
					prefix = "📁 "
				}
				items = append(items, prefix+e.Name())
			}
			return strings.Join(items, "\n"), nil
		},
	}
}

// --- exec ---

var dangerousPatterns = regexp.MustCompile(
	`(?i)(\brm\s+-[rf]{1,2}\b|` +
		`\bdel\s+/[fq]\b|\brmdir\s+/s\b|` +
		`(?:^|[;&|]\s*)format\b|\b(mkfs|diskpart)\b|` +
		`\bdd\s+if=|>\s*/dev/sd|` +
		`\b(shutdown|reboot|poweroff|halt)\b|` +
		`:\(\)\s*\{.*\};\s*:|fork\s*bomb)`,
)

func makeExecTool(workspace, allowedDir string, timeoutSec int, pathAppend string) *AgentTool {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	timeout := time.Duration(timeoutSec) * time.Second
	return &AgentTool{
		name:        "exec",
		description: fmt.Sprintf("Execute a shell command and return its output. Timeout: %ds.", timeoutSec),
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Shell command to execute"},
				"working_dir": map[string]any{"type": "string", "description": "Working directory (optional, defaults to workspace or allowed_dir)"},
			},
			"required": []string{"command"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			command := paramStr(params, "command")
			if dangerousPatterns.MatchString(command) {
				return "", fmt.Errorf("command blocked for safety: %s", command)
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(ctx, "bash", "-c", command)
			if pathAppend != "" {
				cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+string(os.PathListSeparator)+pathAppend)
			}
			// Working directory priority: explicit param > allowedDir > workspace
			if wd := paramStr(params, "working_dir"); wd != "" {
				resolved, err := resolvePath(workspace, wd, allowedDir)
				if err != nil {
					return "", err
				}
				cmd.Dir = resolved
			} else if allowedDir != "" {
				cmd.Dir = allowedDir
			} else {
				cmd.Dir = workspace
			}
			var stdoutBuf, stderrBuf bytes.Buffer
			cmd.Stdout = &stdoutBuf
			cmd.Stderr = &stderrBuf
			runErr := cmd.Run()
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Sprintf("Error: Command timed out after %d seconds", timeoutSec), nil
			}
			var parts []string
			if stdoutBuf.Len() > 0 {
				parts = append(parts, stdoutBuf.String())
			}
			if stderrStr := stderrBuf.String(); strings.TrimSpace(stderrStr) != "" {
				parts = append(parts, "STDERR:\n"+stderrStr)
			}
			if runErr != nil {
				if exitErr, ok := runErr.(*exec.ExitError); ok {
					parts = append(parts, fmt.Sprintf("\nExit code: %d", exitErr.ExitCode()))
				} else {
					parts = append(parts, "\nExit error: "+runErr.Error())
				}
			}
			result := "(no output)"
			if len(parts) > 0 {
				result = strings.Join(parts, "\n")
			}
			// Exec output has a tighter limit than other tools to save tokens.
			if len(result) > 10000 {
				result = result[:10000] + fmt.Sprintf("\n... (truncated, %d more chars)", len(result)-10000)
			}
			return result, nil
		},
	}
}

// --- web_search ---

func makeWebSearchTool() *AgentTool {
	return &AgentTool{
		name:        "web_search",
		description: "Search the web using Brave Search API. Requires BRAVE_API_KEY env var or tools.web_search_api_key config.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
				"count": map[string]any{"type": "integer", "description": "Number of results (default: 5, max: 10)"},
			},
			"required": []string{"query"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			query := paramStr(params, "query")
			count := paramInt(params, "count")
			if count <= 0 || count > 10 {
				count = 5
			}
			apiKey := os.Getenv("BRAVE_API_KEY")
			if apiKey == "" {
				return "", fmt.Errorf("BRAVE_API_KEY not set")
			}

			searchURL := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
				neturl.QueryEscape(query), count)
			req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
			if err != nil {
				return "", fmt.Errorf("build search request: %w", err)
			}
			req.Header.Set("X-Subscription-Token", apiKey)
			req.Header.Set("Accept", "application/json")

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return "", fmt.Errorf("read search response: %w", err)
			}
			if resp.StatusCode != 200 {
				return "", fmt.Errorf("search error %d: %s", resp.StatusCode, string(body))
			}

			var result struct {
				Web struct {
					Results []struct {
						Title       string `json:"title"`
						URL         string `json:"url"`
						Description string `json:"description"`
					} `json:"results"`
				} `json:"web"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Sprintf("Error parsing search results: %v", err), nil
			}

			var sb strings.Builder
			for i, r := range result.Web.Results {
				fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
			}
			if sb.Len() == 0 {
				return "No results found.", nil
			}
			return sb.String(), nil
		},
	}
}

// --- web_fetch ---

func makeWebFetchTool() *AgentTool {
	return &AgentTool{
		name:        "web_fetch",
		description: "Fetch URL and extract readable content (HTML → markdown/text).",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":         map[string]any{"type": "string", "description": "URL to fetch"},
				"extractMode": map[string]any{"type": "string", "enum": []string{"markdown", "text"}, "description": "Extraction mode (default: markdown)"},
				"maxChars":    map[string]any{"type": "integer", "minimum": 100, "description": "Maximum characters to return"},
			},
			"required": []string{"url"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			fetchURL := paramStr(params, "url")
			if fetchURL == "" {
				return "", fmt.Errorf("url is required")
			}
			extractMode := paramStr(params, "extractMode")
			if extractMode == "" {
				extractMode = "markdown"
			}
			maxChars := paramInt(params, "maxChars")

			parsedURL, err := neturl.Parse(fetchURL)
			if err != nil {
				return marshalJSONResult(map[string]any{"error": err.Error(), "url": fetchURL}), nil
			}
			if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
				scheme := parsedURL.Scheme
				if scheme == "" {
					scheme = "none"
				}
				return marshalJSONResult(map[string]any{
					"error": fmt.Sprintf("Only http/https allowed, got '%s'", scheme),
					"url":   fetchURL,
				}), nil
			}

			req, err := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
			if err != nil {
				return marshalJSONResult(map[string]any{"error": err.Error(), "url": fetchURL}), nil
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Cody/1.0)")
			client := &http.Client{
				Timeout: 30 * time.Second,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					if len(via) >= 5 {
						return fmt.Errorf("too many redirects (max 5)")
					}
					return nil
				},
			}
			resp, err := client.Do(req)
			if err != nil {
				return marshalJSONResult(map[string]any{"error": err.Error(), "url": fetchURL}), nil
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
			if err != nil {
				return marshalJSONResult(map[string]any{"error": err.Error(), "url": fetchURL}), nil
			}

			var content, extractor string
			ctype := resp.Header.Get("Content-Type")
			switch {
			case strings.Contains(ctype, "application/json"):
				var parsed any
				if err := json.Unmarshal(body, &parsed); err == nil {
					if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
						content = string(pretty)
					} else {
						content = string(body)
					}
				} else {
					content = string(body)
				}
				extractor = "json"
			case extractMode == "text":
				content = stripHTML(string(body))
				extractor = "text"
			default:
				article, parseErr := readability.FromReader(strings.NewReader(string(body)), parsedURL)
				if parseErr != nil {
					content = stripHTML(string(body))
					extractor = "raw"
				} else {
					content = article.TextContent
					if article.Title != "" {
						content = "# " + article.Title + "\n\n" + content
					}
					extractor = "readability"
				}
			}

			limit := 50000
			if maxChars > 0 {
				limit = maxChars
			}
			truncated := len(content) > limit
			if truncated {
				content = content[:limit]
			}
			finalURL := fetchURL
			if resp.Request != nil && resp.Request.URL != nil {
				finalURL = resp.Request.URL.String()
			}
			result, err := json.Marshal(map[string]any{
				"url":       fetchURL,
				"finalUrl":  finalURL,
				"status":    resp.StatusCode,
				"extractor": extractor,
				"truncated": truncated,
				"length":    len(content),
				"text":      content,
			})
			if err != nil {
				return "", fmt.Errorf("marshal fetch result: %w", err)
			}
			return string(result), nil
		},
	}
}

var (
	reHTMLTags   = regexp.MustCompile(`<[^>]*>`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

func stripHTML(s string) string {
	text := reHTMLTags.ReplaceAllString(s, " ")
	text = reWhitespace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

func marshalJSONResult(payload map[string]any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"error":"failed to marshal JSON response"}`
	}
	return string(data)
}

// findCloseMatch searches content for a line window similar to needle.
// Returns a hint string if a close match (>50% similar) is found.
func findCloseMatch(content, needle string) string {
	contentLines := strings.Split(content, "\n")
	needleLines := strings.Split(needle, "\n")
	windowSize := len(needleLines)
	if windowSize == 0 || len(contentLines) == 0 {
		return ""
	}

	bestRatio := 0.0
	bestStart := 0
	for i := 0; i <= len(contentLines)-windowSize; i++ {
		window := strings.Join(contentLines[i:i+windowSize], "\n")
		ratio := similarity(needle, window)
		if ratio > bestRatio {
			bestRatio = ratio
			bestStart = i
		}
	}
	// Also check single-line similarity if needle is single-line
	if windowSize == 1 {
		for i, line := range contentLines {
			ratio := similarity(needle, line)
			if ratio > bestRatio {
				bestRatio = ratio
				bestStart = i
			}
		}
	}

	if bestRatio < 0.5 {
		return ""
	}
	end := bestStart + windowSize
	if end > len(contentLines) {
		end = len(contentLines)
	}
	actual := strings.Join(contentLines[bestStart:end], "\n")
	return fmt.Sprintf("Best match (%.0f%% similar) at line %d:\n--- provided ---\n%s\n--- actual ---\n%s",
		bestRatio*100, bestStart+1, needle, actual)
}

// similarity returns a ratio (0-1) of how similar two strings are,
// using Levenshtein distance normalized by the longer string length.
func similarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 || lb == 0 {
		return 0.0
	}

	// Levenshtein distance via two-row DP.
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	maxLen := la
	if lb > maxLen {
		maxLen = lb
	}
	return 1.0 - float64(prev[lb])/float64(maxLen)
}

// --- message ---

func makeMessageTool(bus *MessageBus, reqCtx *RequestContext) *AgentTool {
	return &AgentTool{
		name:        "message",
		description: "Send a message to the user. Use this to proactively communicate results or updates.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "Message content to send"},
				"channel": map[string]any{"type": "string", "description": "Optional: target channel (accepted for compatibility)"},
				"chat_id": map[string]any{"type": "string", "description": "Optional: target chat ID"},
				"media":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional list of file paths to attach (images, audio, documents)"},
			},
			"required": []string{"content"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			content := paramStr(params, "content")
			if content == "" {
				return "", fmt.Errorf("content is required")
			}
			targetChatID := reqCtx.ChatID
			if override := paramStr(params, "chat_id"); override != "" {
				targetChatID = override
			}
			if targetChatID == "" {
				return "", fmt.Errorf("no target chat available")
			}
			out := &OutboundMessage{
				ChatID:           targetChatID,
				Content:          content,
				ReplyToMessageID: reqCtx.MessageID,
			}
			// Parse media file paths if provided.
			if rawMedia, ok := params["media"]; ok {
				if mediaList, ok := rawMedia.([]any); ok {
					for _, item := range mediaList {
						if path, ok := item.(string); ok && path != "" {
							out.Media = append(out.Media, Media{Type: "document", URL: path})
						}
					}
				}
			}
			bus.Outbound <- out
			// Suppress final assistant output only when sending to current chat.
			if targetChatID == reqCtx.ChatID {
				reqCtx.MessageSent = true
			}
			if len(out.Media) > 0 {
				return fmt.Sprintf("Message sent to user with %d attachment(s).", len(out.Media)), nil
			}
			return "Message sent to user.", nil
		},
	}
}

// --- cron ---

func makeCronTool(cronSvc *CronService, reqCtx *RequestContext) *AgentTool {
	return &AgentTool{
		name:        "cron",
		description: "Manage scheduled tasks. Actions: add, remove, list, enable, disable.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":        map[string]any{"type": "string", "description": "Action: add, remove, list, enable, disable", "enum": []string{"add", "remove", "list", "enable", "disable"}},
				"name":          map[string]any{"type": "string", "description": "Job name (for add)"},
				"every_seconds": map[string]any{"type": "integer", "description": "Recurring interval in seconds (for add)"},
				"cron_expr":     map[string]any{"type": "string", "description": "Cron expression e.g. '0 9 * * *' (for add)"},
				"at":            map[string]any{"type": "string", "description": "One-time ISO datetime e.g. '2024-01-01T09:00:00Z' (for add)"},
				"tz":            map[string]any{"type": "string", "description": "Timezone for cron expressions (e.g., 'America/New_York'). Optional."},
				"message":       map[string]any{"type": "string", "description": "Task description or reminder text"},
				"deliver":       map[string]any{"type": "boolean", "description": "If true, deliver message to user; if false, execute as agent task"},
				"job_id":        map[string]any{"type": "string", "description": "Job ID (for remove/enable/disable)"},
			},
			"required": []string{"action"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			action := paramStr(params, "action")
			switch action {
			case "list":
				jobs := cronSvc.listJobs()
				if len(jobs) == 0 {
					return "No scheduled jobs.", nil
				}
				var sb strings.Builder
				for _, j := range jobs {
					status := "enabled"
					if !j.Enabled {
						status = "disabled"
					}
					fmt.Fprintf(&sb, "- [%s] %s (%s) schedule=%s next=%s\n",
						j.ID, j.Name, status, j.Schedule.Raw, j.State.NextRunAt.Format(time.RFC3339))
				}
				return sb.String(), nil

			case "add":
				name := paramStr(params, "name")
				message := paramStr(params, "message")
				// Match nanobot behavior: cron jobs created via tool calls deliver by default.
				deliver := true
				if rawDeliver, ok := params["deliver"]; ok {
					if b, ok := rawDeliver.(bool); ok {
						deliver = b
					}
				}
				tz := paramStr(params, "tz")

				// Build schedule string from nanobot-style params.
				everySeconds := paramInt(params, "every_seconds")
				cronExpr := paramStr(params, "cron_expr")
				at := paramStr(params, "at")

				var schedule string
				switch {
				case everySeconds > 0:
					schedule = fmt.Sprintf("every %ds", everySeconds)
				case cronExpr != "":
					schedule = cronExpr
				case at != "":
					schedule = "at " + at
				default:
					return "", fmt.Errorf("one of every_seconds, cron_expr, or at is required for add")
				}

				if name == "" {
					name = message
					if len(name) > 50 {
						name = name[:50]
					}
				}
				if message == "" {
					return "", fmt.Errorf("message is required for add")
				}
				// Validate timezone if provided.
				if tz != "" {
					if _, err := time.LoadLocation(tz); err != nil {
						return "", fmt.Errorf("invalid timezone %q: %v", tz, err)
					}
				}
				job, err := cronSvc.addJob(name, schedule, message, deliver, tz, reqCtx.ChatID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Job created: %s (id=%s, next=%s)", job.Name, job.ID, job.State.NextRunAt.Format(time.RFC3339)), nil

			case "remove":
				id := paramStr(params, "job_id")
				if cronSvc.removeJob(id) {
					return "Job removed.", nil
				}
				return "", fmt.Errorf("job not found: %s", id)

			case "enable":
				id := paramStr(params, "job_id")
				if cronSvc.enableJob(id, true) {
					return "Job enabled.", nil
				}
				return "", fmt.Errorf("job not found: %s", id)

			case "disable":
				id := paramStr(params, "job_id")
				if cronSvc.enableJob(id, false) {
					return "Job disabled.", nil
				}
				return "", fmt.Errorf("job not found: %s", id)

			default:
				return "", fmt.Errorf("unknown action: %s", action)
			}
		},
	}
}

// --- spawn ---

func makeSpawnTool(subMgr *SubagentManager, reqCtx *RequestContext) *AgentTool {
	return &AgentTool{
		name:        "spawn",
		description: "Spawn a background task that runs independently. Returns a task ID. Results are delivered when complete.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task":  map[string]any{"type": "string", "description": "Detailed description of the task to perform"},
				"label": map[string]any{"type": "string", "description": "Short label for the task"},
			},
			"required": []string{"task"},
		},
		execute: func(ctx context.Context, params map[string]any) (string, error) {
			task := paramStr(params, "task")
			label := paramStr(params, "label")
			if label == "" {
				label = task[:min(50, len(task))]
			}
			slog.Info("Spawning subagent", "label", label)
			id := subMgr.spawn(task, label, reqCtx.ChatID, reqCtx.SessionKey)
			return fmt.Sprintf("Subagent [%s] started (id: %s). I'll notify you when it completes.", label, id), nil
		},
	}
}
