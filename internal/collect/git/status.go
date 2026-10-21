package git

import (
	"fmt"
	"strings"
)

// parseStatusV2 interprets "git status --porcelain=v2 -z --branch" output.
// Every record is NUL-terminated; a rename/copy record ("2") is followed by a
// second NUL-terminated field holding the original path, which is skipped.
func parseStatusV2(out []byte) (Head, Changes, error) {
	var head Head
	var ch Changes
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		switch {
		case strings.HasPrefix(rec, "# branch.oid "):
			v := strings.TrimPrefix(rec, "# branch.oid ")
			if v == "(initial)" {
				head.Unborn = true
			} else {
				head.Commit = v
			}
		case strings.HasPrefix(rec, "# branch.head "):
			v := strings.TrimPrefix(rec, "# branch.head ")
			if v == "(detached)" {
				head.Detached = true
			} else {
				head.Branch = v
			}
		case strings.HasPrefix(rec, "# "):
			// other headers (upstream, ahead/behind, stash) are not used
		case strings.HasPrefix(rec, "1 "), strings.HasPrefix(rec, "2 "):
			fields := strings.SplitN(rec, " ", 3)
			if len(fields) < 3 || len(fields[1]) != 2 {
				return head, ch, fmt.Errorf("status: malformed record %q", truncate(rec))
			}
			countXY(&ch, fields[1])
			if rec[0] == '2' {
				i++ // skip the original path of a rename or copy
			}
		case strings.HasPrefix(rec, "u "):
			ch.Tracked++
			ch.Conflicted++
		case strings.HasPrefix(rec, "? "):
			ch.Untracked++
		case strings.HasPrefix(rec, "! "):
			// ignored entries are not requested and never counted
		default:
			return head, ch, fmt.Errorf("status: unexpected record %q", truncate(rec))
		}
	}
	if head.Commit == "" && !head.Unborn {
		return head, ch, fmt.Errorf("status: no branch.oid header")
	}
	return head, ch, nil
}

func countXY(ch *Changes, xy string) {
	ch.Tracked++
	if xy[0] != '.' {
		ch.Staged++
	}
	if xy[1] != '.' {
		ch.Unstaged++
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
