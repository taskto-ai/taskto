package skills

import (
	"context"
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestSkillListsAreNeverNil(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for name, o := range map[string]Owner{
		"user":      w.owner(ScopeUser, w.bob, 0),
		"space":     w.owner(ScopeSpace, 0, w.team),
		"workspace": w.owner(ScopeWorkspace, 0, 0),
	} {
		for _, all := range []bool{true, false} {
			list, err := w.store.List(ctx, o, all)
			if err != nil || len(list) != 0 {
				t.Fatalf("%s: %v, %v", name, list, err)
			}
			nonnull.Require(t, SkillList{Skills: list})
		}
	}
	list, err := w.store.ForUser(ctx, w.other, w.bob)
	if err != nil || len(list) != 0 {
		t.Fatalf("a non-member: %v, %v", list, err)
	}
	nonnull.Require(t, SkillList{Skills: list})
}
