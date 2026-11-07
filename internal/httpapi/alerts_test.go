package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// postRule writes a rule through the API of the administrator.
func postRule(t *testing.T, server *authServer, csrf string, payload map[string]any) *http.Response {
	t.Helper()

	return server.do(http.MethodPost, "/api/v1/alert-rules", payload,
		withOrigin(server.server.URL), withHeader("X-CSRF-Token", csrf))
}

// putRule changes one rule through the API of the administrator.
func putRule(t *testing.T, server *authServer, csrf, ruleID string, payload map[string]any) *http.Response {
	t.Helper()

	return server.do(http.MethodPut, "/api/v1/alert-rules/"+ruleID, payload,
		withOrigin(server.server.URL), withHeader("X-CSRF-Token", csrf))
}

// TestTheAlertRuleSavesItsNameAndSeverity proves that the profile of a
// rule travels through the API: the answer of the creation carries the
// name and the severity, the list carries them and a change keeps them.
func TestTheAlertRuleSavesItsNameAndSeverity(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := postRule(t, server, csrf, map[string]any{
		"name":        "容器CPU使用率告警",
		"kind":        "resource",
		"severity":    "critical",
		"scope_id":    "container-1",
		"metric":      "cpu_percent",
		"threshold":   80,
		"for_seconds": 60,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	payload := server.decode(response)
	if payload["name"] != "容器CPU使用率告警" {
		t.Fatalf("the answer holds the name %v, want the name of the request", payload["name"])
	}
	if payload["severity"] != "critical" {
		t.Fatalf("the answer holds the severity %v, want critical", payload["severity"])
	}
	ruleID, _ := payload["id"].(string)
	if ruleID == "" {
		t.Fatal("the answer holds no identifier of the rule")
	}

	response = server.get("/api/v1/alert-rules")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body := readBody(t, response)
	if !strings.Contains(body, "容器CPU使用率告警") || !strings.Contains(body, `"severity":"critical"`) {
		t.Fatalf("the list holds no profile of the rule: %s", body)
	}

	response = putRule(t, server, csrf, ruleID, map[string]any{
		"name":        "容器内存告警",
		"kind":        "resource",
		"severity":    "info",
		"scope_id":    "container-1",
		"metric":      "cpu_percent",
		"threshold":   90,
		"for_seconds": 30,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload = server.decode(response)
	if payload["name"] != "容器内存告警" || payload["severity"] != "info" {
		t.Fatalf("the change lost the profile: %v", payload)
	}
}

// TestTheAlertRuleDefaultsToAWarning proves that a rule without a severity
// is a warning, so an old caller keeps the behaviour it had.
func TestTheAlertRuleDefaultsToAWarning(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := postRule(t, server, csrf, map[string]any{
		"name":     "容器异常退出告警",
		"kind":     "container_exit",
		"scope_id": "container-1",
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	payload := server.decode(response)
	if payload["severity"] != "warning" {
		t.Fatalf("the severity = %v, want warning", payload["severity"])
	}
}

// TestTheAlertRuleNeedsAName proves that a rule without a name is refused.
//
// The name is the title the administrator reads in the list of the rules,
// so a rule without one has no face.
func TestTheAlertRuleNeedsAName(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := postRule(t, server, csrf, map[string]any{
		"kind":      "resource",
		"scope_id":  "container-1",
		"metric":    "cpu_percent",
		"threshold": 80,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload := server.decode(response)
	if payload["code"] != "validation_failed" {
		t.Fatalf("the error = %v, want a validation failure", payload)
	}
	fields, _ := payload["fields"].(map[string]any)
	if fields["name"] == "" {
		t.Fatalf("the error names no field of the name: %v", payload)
	}
}

// TestTheAlertRuleRefusesAnUnknownSeverity proves that a severity the
// console does not know is refused instead of saved.
func TestTheAlertRuleRefusesAnUnknownSeverity(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := postRule(t, server, csrf, map[string]any{
		"name":      "容器CPU使用率告警",
		"kind":      "resource",
		"severity":  "urgent",
		"scope_id":  "container-1",
		"metric":    "cpu_percent",
		"threshold": 80,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload := server.decode(response)
	fields, _ := payload["fields"].(map[string]any)
	if fields["severity"] == "" {
		t.Fatalf("the error names no field of the severity: %v", payload)
	}
}

// TestTheAlertRuleSavesItsScopeAndCondition proves that the breadth of a
// rule, the direction of its comparison and its description travel through
// the API.
func TestTheAlertRuleSavesItsScopeAndCondition(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	// A rule of all containers names no object and compares at or above.
	response := postRule(t, server, csrf, map[string]any{
		"name":           "全部容器CPU告警",
		"description":    "任一容器 CPU 过载时告警",
		"kind":           "resource",
		"scope_kind":     "all",
		"metric":         "cpu_percent",
		"operator":       ">=",
		"threshold":      80,
		"for_seconds":    60,
		"recover_notify": false,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	payload := server.decode(response)
	if payload["scope_kind"] != "all" || payload["operator"] != ">=" || payload["description"] != "任一容器 CPU 过载时告警" {
		t.Fatalf("the answer lost the scope and the condition: %v", payload)
	}
	if payload["recover_notify"] != false {
		t.Fatalf("the answer reports a recovery that speaks: %v", payload["recover_notify"])
	}
	if payload["scope_id"] != "" {
		t.Fatalf("a rule of all carries the object %v", payload["scope_id"])
	}
	ruleID, _ := payload["id"].(string)

	// The rule of one namespace names the namespace.
	response = postRule(t, server, csrf, map[string]any{
		"name":       "命名空间CPU告警",
		"kind":       "resource",
		"scope_kind": "namespace",
		"scope_id":   "web",
		"metric":     "cpu_percent",
		"threshold":  80,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	payload = server.decode(response)
	if payload["scope_kind"] != "namespace" || payload["scope_id"] != "web" {
		t.Fatalf("the answer lost the namespace: %v", payload)
	}

	// The list carries the profile of every rule. The JSON of the answer
	// escapes the comparison signs, so the assertion reads the escaped
	// form of ">=".
	response = server.get("/api/v1/alert-rules")
	defer response.Body.Close()
	body := readBody(t, response)
	if !strings.Contains(body, `"scope_kind":"all"`) || !strings.Contains(body, `"operator":"\u003e="`) {
		t.Fatalf("the list holds no scope and condition: %s", body)
	}

	// The change keeps the profile of the rule.
	response = putRule(t, server, csrf, ruleID, map[string]any{
		"name":           "全部容器CPU告警",
		"description":    "新描述",
		"kind":           "resource",
		"scope_kind":     "all",
		"metric":         "cpu_percent",
		"operator":       "<",
		"threshold":      5,
		"for_seconds":    60,
		"recover_notify": true,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload = server.decode(response)
	if payload["operator"] != "<" || payload["description"] != "新描述" || payload["recover_notify"] != true {
		t.Fatalf("the change lost the profile: %v", payload)
	}
}

// TestTheAlertRuleRefusesABrokenScopeAndCondition proves that the parts of
// a rule the console does not know are refused.
func TestTheAlertRuleRefusesABrokenScopeAndCondition(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	// An unknown operator and an unknown scope are refused.
	for _, broken := range []map[string]any{
		{
			"name": "规则", "kind": "resource", "scope_id": "c1",
			"metric": "cpu_percent", "threshold": 80, "operator": "equals",
		},
		{
			"name": "规则", "kind": "resource", "scope_id": "c1",
			"metric": "cpu_percent", "threshold": 80, "scope_kind": "cluster",
		},
	} {
		response := postRule(t, server, csrf, broken)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("the broken rule was accepted: %d", response.StatusCode)
		}
		response.Body.Close()
	}

	// A rule of one container names its object.
	response := postRule(t, server, csrf, map[string]any{
		"name": "规则", "kind": "resource", "scope_kind": "container",
		"metric": "cpu_percent", "threshold": 80,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload := server.decode(response)
	fields, _ := payload["fields"].(map[string]any)
	if fields["scope_id"] == "" {
		t.Fatalf("the error names no field of the object: %v", payload)
	}

	// A description that is too long is refused.
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'a'
	}
	response = postRule(t, server, csrf, map[string]any{
		"name": "规则", "kind": "resource", "scope_id": "c1",
		"metric": "cpu_percent", "threshold": 80, "description": string(long),
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload = server.decode(response)
	fields, _ = payload["fields"].(map[string]any)
	if fields["description"] == "" {
		t.Fatalf("the error names no field of the description: %v", payload)
	}
}
