package registry

import "testing"

// TestWebSearchIndexProviderIsolation proves the index keeps per-provider cells
// separate from the aggregate: one provider's true must not leak into another
// provider's view, while the aggregate still ORs them.
func TestWebSearchIndexProviderIsolation(t *testing.T) {
	r := newTestModelRegistry()

	r.RegisterClient("client-A", "provider-a", []*ModelInfo{{
		ID:                 "shared-model",
		NativeCapabilities: nativeCaps(true),
	}})
	r.RegisterClient("client-B", "provider-b", []*ModelInfo{{
		ID:                 "shared-model",
		NativeCapabilities: nativeCaps(false),
	}})

	if info := r.GetModelInfo("shared-model", "provider-a"); !webSearchSupported(info) {
		t.Fatalf("provider-a view: expected WebSearch=true, got %+v", info)
	}
	if info := r.GetModelInfo("shared-model", "provider-b"); info == nil || webSearchSupported(info) {
		t.Fatalf("provider-b view: expected WebSearch=false, got %+v", info)
	}
	if info := r.GetModelInfo("shared-model", ""); !webSearchSupported(info) {
		t.Fatalf("aggregate view: expected WebSearch=true, got %+v", info)
	}

	// Dropping the true provider must leave provider-b's false intact.
	r.UnregisterClient("client-A")
	if info := r.GetModelInfo("shared-model", "provider-b"); info == nil || webSearchSupported(info) {
		t.Fatalf("provider-b view after unregister: expected WebSearch=false, got %+v", info)
	}
	if info := r.GetModelInfo("shared-model", ""); info == nil || webSearchSupported(info) {
		t.Fatalf("aggregate view after unregister: expected WebSearch=false, got %+v", info)
	}
}

// TestWebSearchIndexClearsOrphanedClientInfos proves a client that holds
// clientModelInfos but no clientModels entry (the orphan state upstream's
// 58874f3bf392 guards) is retracted from the index instead of leaking counts.
func TestWebSearchIndexClearsOrphanedClientInfos(t *testing.T) {
	r := newTestModelRegistry()

	r.RegisterClient("client-A", "provider-a", []*ModelInfo{{
		ID:                 "orphan-model",
		NativeCapabilities: nativeCaps(true),
	}})
	// Simulate the orphan state: metadata remains, model bookkeeping is gone.
	delete(r.clientModels, "client-A")

	r.UnregisterClient("client-A")

	if got := r.webSearchCountLocked(r.webSearchTrueCounts, "orphan-model", ""); got != 0 {
		t.Fatalf("aggregate true count after orphan cleanup = %d, want 0", got)
	}
	if got := r.webSearchCountLocked(r.webSearchTrueCounts, "orphan-model", "provider-a"); got != 0 {
		t.Fatalf("provider true count after orphan cleanup = %d, want 0", got)
	}
	if _, ok := r.clientModelInfos["client-A"]; ok {
		t.Fatal("orphaned clientModelInfos entry survived cleanup")
	}
}

// TestWebSearchIndexPrunesEmptyCells proves the index does not retain empty
// maps after every client unregisters, so a long-lived registry cannot grow
// without bound.
func TestWebSearchIndexPrunesEmptyCells(t *testing.T) {
	r := newTestModelRegistry()

	r.RegisterClient("client-A", "provider-a", []*ModelInfo{{
		ID:                 "pruned-model",
		NativeCapabilities: nativeCaps(true),
	}})
	r.RegisterClient("client-B", "provider-b", []*ModelInfo{{
		ID:                 "pruned-model",
		NativeCapabilities: nativeCaps(false),
	}})

	r.UnregisterClient("client-A")
	r.UnregisterClient("client-B")

	if len(r.webSearchTrueCounts) != 0 {
		t.Fatalf("true counts not pruned: %+v", r.webSearchTrueCounts)
	}
	if len(r.webSearchFalseCounts) != 0 {
		t.Fatalf("false counts not pruned: %+v", r.webSearchFalseCounts)
	}
}
