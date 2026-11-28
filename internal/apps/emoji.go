package apps

import "sort"

const (
	zeroWidthJoiner  = 0x200D
	variationEmoji   = 0xFE0F
	combiningKeycap  = 0x20E3
	skinToneFirst    = 0x1F3FB
	skinToneLast     = 0x1F3FF
	regionalFirst    = 0x1F1E6
	regionalLast     = 0x1F1FF
	blackFlag        = 0x1F3F4
	tagFirst         = 0xE0020
	tagLast          = 0xE007E
	tagCancel        = 0xE007F
	minimumKeycapLen = 2
)

func isPictographic(r rune) bool {
	i := sort.Search(len(pictographic), func(i int) bool { return pictographic[i][1] >= r })
	return i < len(pictographic) && pictographic[i][0] <= r
}

func isRegional(r rune) bool {
	return r >= regionalFirst && r <= regionalLast
}

func isSkinTone(r rune) bool {
	return r >= skinToneFirst && r <= skinToneLast
}

func isKeycapBase(r rune) bool {
	return (r >= '0' && r <= '9') || r == '#' || r == '*'
}

func isEmoji(s string) bool {
	rs := []rune(s)
	return isKeycap(rs) || isFlag(rs) || isTagSequence(rs) || isJoinedPictographs(rs)
}

func isKeycap(rs []rune) bool {
	if len(rs) < minimumKeycapLen || len(rs) > minimumKeycapLen+1 || !isKeycapBase(rs[0]) || rs[len(rs)-1] != combiningKeycap {
		return false
	}
	return len(rs) == minimumKeycapLen || rs[1] == variationEmoji
}

func isFlag(rs []rune) bool {
	return len(rs) == 2 && isRegional(rs[0]) && isRegional(rs[1])
}

func isTagSequence(rs []rune) bool {
	if len(rs) < 3 || rs[0] != blackFlag || rs[len(rs)-1] != tagCancel {
		return false
	}
	for _, r := range rs[1 : len(rs)-1] {
		if r < tagFirst || r > tagLast {
			return false
		}
	}
	return true
}

func isJoinedPictographs(rs []rune) bool {
	for i := 0; ; {
		if i >= len(rs) || !isPictographic(rs[i]) {
			return false
		}
		i++
		if i < len(rs) && (rs[i] == variationEmoji || isSkinTone(rs[i])) {
			i++
		}
		if i == len(rs) {
			return true
		}
		if rs[i] != zeroWidthJoiner {
			return false
		}
		i++
	}
}
