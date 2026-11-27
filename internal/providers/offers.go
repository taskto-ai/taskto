package providers

import (
	"context"
	"strconv"
)

func (s *Service) Offers(ctx context.Context, provider, model string, user, workspace int64) (bool, error) {
	if !Usable(provider, user, workspace) {
		return false, nil
	}
	var choices []Choice
	if m := providerRe.FindStringSubmatch(provider); m == nil {
		if s.feed != nil {
			choices = s.feed.Instance()
		}
	} else {
		id, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return false, err
		}
		o := owner{user: id}
		if m[1] == "ws" {
			o = owner{workspace: id}
		}
		recs, err := s.store.list(ctx, o)
		if err != nil {
			return false, err
		}
		for _, rec := range recs {
			choices = append(choices, choiceOf(rec))
		}
	}
	for _, c := range choices {
		if c.Provider != provider {
			continue
		}
		for _, m := range c.Models {
			if m.ID == model {
				return true, nil
			}
		}
	}
	return false, nil
}
