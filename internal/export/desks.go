package export

import (
	"context"
	"strconv"
)

func (ex *run) desks(ctx context.Context) error {
	if ex.svc.desks == nil {
		ex.skip(place{}, "desks", "off", "")
		return nil
	}
	list, err := ex.svc.desks.Export(ctx, ex.ws.WorkspaceID, ex.user.ID)
	if err != nil {
		return err
	}
	for _, d := range list {
		label := "main"
		if d.Name != "" {
			label = slug(d.Name)
		}
		name := ex.names.claim("desks", cleanElem(strconv.FormatInt(d.ID, 10)+"-"+label, maxElemBytes)+".json")
		if err := ex.json(name, d); err != nil {
			return err
		}
		ex.mf.Counts["desks"]++
	}
	return nil
}
