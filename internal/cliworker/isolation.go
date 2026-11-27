package cliworker

import "fmt"

const minUIDBase = 65536

const maxUID = 1<<32 - 2

const maxUIDBase = maxUID - maxHome

func (i *Isolation) uidOf(home int64) (int, error) {
	if home < firstHome || home >= maxHome {
		return 0, fmt.Errorf("home %d can't have a Unix user", home)
	}
	return i.UIDBase + int(home), nil
}

func checkUIDBase(base int) error {
	if base < minUIDBase {
		return fmt.Errorf("cliworker: the first uid must be %d or more", minUIDBase)
	}
	if base > maxUIDBase {
		return fmt.Errorf("cliworker: the first uid must be %d or less", maxUIDBase)
	}
	return nil
}
