package yamlspec_test

import (
	"testing"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/yamlspec"
)

func TestTheNoticeOfAVariableIsNoRisk(t *testing.T) {
	kind := yamlspec.WarningKindOf("变量 TAG 没有提供，将按空字符串处理")
	if kind != yamlspec.WarningVariable {
		t.Fatalf("kind = %q, want %q", kind, yamlspec.WarningVariable)
	}
}

func TestTheMessageOfAServiceIsNoRisk(t *testing.T) {
	// The message of a service that refers to a resource the document does
	// not declare describes a configuration the platform refuses, so it asks
	// for no confirmation.
	kind := yamlspec.WarningKindOf("服务 X 引用了未声明的 Y")
	if kind == yamlspec.WarningRisk {
		t.Fatalf("kind = %q, want no risk", kind)
	}
	if kind != yamlspec.WarningNotice {
		t.Fatalf("kind = %q, want %q", kind, yamlspec.WarningNotice)
	}
}

func TestTheRisksOfTheConfigurationAreRisks(t *testing.T) {
	messages := []string{
		"服务 api 以特权模式运行",
		"服务 api 共享宿主机的 PID 命名空间",
		"服务 api 共享宿主机网络命名空间",
		"服务 api 挂载了 Docker socket",
		"服务 api 挂载了敏感的宿主机路径 /var/lib/docker",
	}
	for _, message := range messages {
		if kind := yamlspec.WarningKindOf(message); kind != yamlspec.WarningRisk {
			t.Fatalf("the kind of %q = %q, want %q", message, kind, yamlspec.WarningRisk)
		}
	}
}

func TestAnUnknownMessageIsARisk(t *testing.T) {
	// A message the platform does not know is a risk: the administrator
	// confirms it, so a new risk is never accepted silently.
	kind := yamlspec.WarningKindOf("服务 api 做了平台还不知道的事")
	if kind != yamlspec.WarningRisk {
		t.Fatalf("kind = %q, want %q", kind, yamlspec.WarningRisk)
	}
}

func TestTheWarningsOfTheConfigurationKnowTheirKind(t *testing.T) {
	app := model.App{
		ID:        "app-1",
		Name:      "demo",
		Mode:      yamlspec.ModeCompose,
		TargetID:  "node-1",
		Namespace: "demo",
	}

	risky, err := yamlspec.Check(app, model.Config{
		YAML: "services:\n  api:\n    image: nginx:alpine\n    privileged: true\n",
	})
	if err != nil {
		t.Fatalf("check the configuration: %v", err)
	}
	if len(risky.Warnings) == 0 {
		t.Fatal("the configuration holds no warning")
	}
	for _, warning := range risky.Warnings {
		if kind := yamlspec.WarningKindOf(warning); kind != yamlspec.WarningRisk {
			t.Fatalf("the kind of %q = %q, want %q", warning, kind, yamlspec.WarningRisk)
		}
	}

	unset, err := yamlspec.Check(app, model.Config{
		YAML: "services:\n  api:\n    image: nginx:alpine\n    command: [\"echo\", \"$TAG\"]\n",
	})
	if err != nil {
		t.Fatalf("check the configuration: %v", err)
	}
	if len(unset.Warnings) == 0 {
		t.Fatal("the configuration holds no warning")
	}
	for _, warning := range unset.Warnings {
		if kind := yamlspec.WarningKindOf(warning); kind != yamlspec.WarningVariable {
			t.Fatalf("the kind of %q = %q, want %q", warning, kind, yamlspec.WarningVariable)
		}
	}
}
