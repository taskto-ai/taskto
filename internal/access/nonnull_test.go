package access

import (
	"testing"

	"taskto.ai/taskto/internal/apitypes/nonnull"
)

func TestAppAccessRolesAreNeverNil(t *testing.T) {
	for _, sp := range []Access{None, Read, Write} {
		for _, restricted := range []bool{false, true} {
			for _, roles := range [][]AppRole{nil, {}, {AppReader}, AppRoles} {
				for _, rescue := range []bool{false, true} {
					nonnull.Require(t, appAccess(sp, roles, restricted, rescue))
				}
			}
		}
	}
}
