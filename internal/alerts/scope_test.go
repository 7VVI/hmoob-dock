package alerts_test

import (
	"context"
	"testing"
	"time"

	"hmoob-dock/internal/alerts"
)

// TestTheOperatorChoosesTheDirectionOfTheRule proves that the operator of a
// resource rule compares the value in the direction it names.
func TestTheOperatorChoosesTheDirectionOfTheRule(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1800000000, 0)

	cases := []struct {
		operator string
		value    float64
		opens    bool
	}{
		{alerts.OperatorGreaterThan, 81, true},
		{alerts.OperatorGreaterThan, 80, false},
		{alerts.OperatorGreaterOrEqual, 80, true},
		{alerts.OperatorGreaterOrEqual, 79, false},
		{alerts.OperatorLessThan, 79, true},
		{alerts.OperatorLessThan, 80, false},
		{alerts.OperatorLessOrEqual, 80, true},
		{alerts.OperatorLessOrEqual, 81, false},
	}

	for _, item := range cases {
		engine, _ := newEngine(t)
		rule := alerts.Rule{
			ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent",
			Threshold: 80, Operator: item.operator, Enabled: true,
		}
		if err := engine.PutRule(ctx, rule); err != nil {
			t.Fatal(err)
		}
		event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: item.value, Valid: true})
		if err != nil {
			t.Fatal(err)
		}
		if item.opens && event == nil {
			t.Fatalf("operator %s did not fire at %g", item.operator, item.value)
		}
		if !item.opens && event != nil {
			t.Fatalf("operator %s fired at %g", item.operator, item.value)
		}
	}
}

// TestTheQuietRecoveryResolvesWithoutANotification proves that a rule with
// a quiet recovery resolves its event without a notification of its own.
func TestTheQuietRecoveryResolvesWithoutANotification(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{
		ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent",
		Threshold: 80, Enabled: true, RecoverQuiet: true,
	}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	resolved, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(10 * time.Second), Value: 10, Valid: true})
	if err != nil || resolved == nil || resolved.State != alerts.StateResolved {
		t.Fatalf("the alert did not resolve: %v", err)
	}
	if count := countNotifications(t, db); count != 1 {
		t.Fatalf("notifications = %d, want the one of the opening", count)
	}
}

// TestTheRecoveryOfARuleSpeaksByDefault proves that a rule whose recovery
// was not configured keeps a recovery that speaks.
func TestTheRecoveryOfARuleSpeaksByDefault(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(10 * time.Second), Value: 10, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if count := countNotifications(t, db); count != 2 {
		t.Fatalf("notifications = %d, want the opening and the recovery", count)
	}
}

// TestTheExitRulesMatchTheirScope proves that the end of a container falls
// into the scope its rule names.
func TestTheExitRulesMatchTheirScope(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1800000000, 0)

	// A rule of all watches every container.
	engine, _ := newEngine(t)
	if err := engine.PutRule(ctx, alerts.Rule{
		ID: "r-all", Kind: alerts.KindContainerExit, ScopeKind: alerts.ScopeAll, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if event, err := engine.ObserveExit(ctx, alerts.ExitSample{
		NodeID: "n1", ObjectID: "container:c9", Namespace: "web", At: at,
	}); err != nil || event == nil {
		t.Fatalf("a rule of all did not watch the container: %v", err)
	}

	// A rule of a namespace watches the containers of its namespace and
	// never a container whose namespace was unknown.
	engine, _ = newEngine(t)
	if err := engine.PutRule(ctx, alerts.Rule{
		ID: "r-ns", Kind: alerts.KindContainerExit, ScopeKind: alerts.ScopeNamespace,
		ScopeID: "web", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if event, err := engine.ObserveExit(ctx, alerts.ExitSample{
		NodeID: "n1", ObjectID: "container:c1", Namespace: "web", At: at,
	}); err != nil || event == nil {
		t.Fatalf("a rule of a namespace did not watch its namespace: %v", err)
	}
	if event, err := engine.ObserveExit(ctx, alerts.ExitSample{
		NodeID: "n1", ObjectID: "container:c2", Namespace: "db", At: at,
	}); err != nil || event != nil {
		t.Fatalf("a rule of a namespace watched another namespace: %v", err)
	}
	if event, err := engine.ObserveExit(ctx, alerts.ExitSample{
		NodeID: "n1", ObjectID: "container:c3", Namespace: "", At: at,
	}); err != nil || event != nil {
		t.Fatalf("a rule of a namespace watched an unknown namespace: %v", err)
	}

	// A rule of one container watches the object of its scope.
	engine, _ = newEngine(t)
	if err := engine.PutRule(ctx, alerts.Rule{
		ID: "r-c1", Kind: alerts.KindContainerExit, ScopeKind: alerts.ScopeContainer,
		ScopeID: "container:c1", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if event, err := engine.ObserveExit(ctx, alerts.ExitSample{
		NodeID: "n1", ObjectID: "container:c1", At: at,
	}); err != nil || event == nil {
		t.Fatalf("a rule of one container did not watch its object: %v", err)
	}
	if event, err := engine.ObserveExit(ctx, alerts.ExitSample{
		NodeID: "n1", ObjectID: "container:c2", At: at,
	}); err != nil || event != nil {
		t.Fatalf("a rule of one container watched another object: %v", err)
	}
}
