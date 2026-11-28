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
		for _, actor := range []int64{w.bob, w.ann} {
			if o.Scope == ScopeUser && actor != o.UserID {
				continue
			}
			if o.Scope == ScopeSpace && actor != w.bob {
				continue
			}
			list, err := w.store.List(ctx, o, actor)
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
