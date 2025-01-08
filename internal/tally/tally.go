// Handles summations over commits.
package tally

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/sinclairtarget/git-who/internal/git"
	"github.com/sinclairtarget/git-who/internal/timeutils"
)

// Whether we rank authors by commit, lines, or files.
type TallyMode int

const (
	CommitMode TallyMode = iota
	LinesMode
	FilesMode
	LastModifiedMode
)

type TallyOpts struct {
	Mode TallyMode
	Key  func(c git.Commit) string // Unique ID for author
}

// Whether we need --stat and --summary data from git log for this tally mode
func (opts TallyOpts) IsDiffMode() bool {
	return opts.Mode == FilesMode || opts.Mode == LinesMode
}

// Metrics tallied while walking git log
type Tally struct {
	AuthorName     string
	AuthorEmail    string
	Commits        int // Num commits editing paths in tree by this author
	LinesAdded     int // Num lines added to paths in tree by author
	LinesRemoved   int // Num lines deleted from paths in tree by author
	FileCount      int // Num of file paths in working dir touched by author
	LastCommitTime time.Time
}

func (t Tally) SortKey(mode TallyMode) int64 {
	switch mode {
	case CommitMode:
		return int64(t.Commits)
	case FilesMode:
		return int64(t.FileCount)
	case LinesMode:
		return int64(t.LinesAdded + t.LinesRemoved)
	case LastModifiedMode:
		return t.LastCommitTime.Unix()
	default:
		panic("unrecognized mode in switch statement")
	}
}

func (a Tally) Compare(b Tally, mode TallyMode) int {
	aRank := a.SortKey(mode)
	bRank := b.SortKey(mode)

	if aRank < bRank {
		return -1
	} else if bRank < aRank {
		return 1
	}

	// Break ties with last edited
	return a.LastCommitTime.Compare(b.LastCommitTime)
}

// A tally that can be combined with other tallies
type intermediateTally struct {
	commitset      map[string]bool
	added          int
	removed        int
	lastCommitTime time.Time
	numTallied     int
}

func newTally(numTallied int) intermediateTally {
	return intermediateTally{
		commitset:  map[string]bool{},
		numTallied: numTallied,
	}
}

func (t intermediateTally) Commits() int {
	return len(t.commitset)
}

func (a intermediateTally) Add(b intermediateTally) intermediateTally {
	union := a.commitset
	for commit, _ := range b.commitset {
		union[commit] = true
	}

	return intermediateTally{
		commitset:      union,
		added:          a.added + b.added,
		removed:        a.removed + b.removed,
		lastCommitTime: timeutils.Max(a.lastCommitTime, b.lastCommitTime),
		numTallied:     a.numTallied + b.numTallied,
	}
}

// Returns a slice of tallies, each one for a different author, in descending
// order by most commits / files / lines (depending on the tally mode).
func TallyCommits(
	commits iter.Seq2[git.Commit, error],
	wtreefiles map[string]bool,
	allowOutsideWorktree bool,
	opts TallyOpts,
) ([]Tally, error) {
	authorTallies, err := tallyCommits(
		commits,
		wtreefiles,
		allowOutsideWorktree,
		opts,
	)
	if err != nil {
		return nil, err
	}

	// Sort list
	sorted := sortTallies(authorTallies, opts.Mode)
	return sorted, nil
}

// Concurrent pipeline for simple tallying of commits
func TallyCommitsApplyMerge(
	wtreeset map[string]bool,
	allowOutsideWorktree bool,
	opts TallyOpts,
) (
	TallyFunc[map[string]Tally],
	MergeFunc[map[string]Tally],
	FinalizeFunc[map[string]Tally, []Tally],
) {
	apply := func(commits iter.Seq2[git.Commit, error]) (
		map[string]Tally,
		error,
	) {
		if opts.IsDiffMode() {
			return nil, errors.New("unsupported tally mode")
		}

		return tallyCommits(
			commits,
			wtreeset,
			allowOutsideWorktree,
			opts,
		)
	}

	merge := func(a, b map[string]Tally) map[string]Tally {
		union := b
		for k, at := range a {
			bt := union[k]

			at.Commits += bt.Commits
			at.LastCommitTime = timeutils.Max(
				at.LastCommitTime,
				bt.LastCommitTime,
			)

			union[k] = at
		}
		return union
	}

	finalize := func(tallies map[string]Tally) []Tally {
		return sortTallies(tallies, opts.Mode)
	}

	return apply, merge, finalize
}

// Concurrent pipeline for tallying commits using diffs (lines, files)
func TallyCommitsDiffApplyMerge(
	wtreeset map[string]bool,
	allowOutsideWorktree bool,
	opts TallyOpts,
) (
	TallyFunc[map[string]AuthorPaths],
	MergeFunc[map[string]AuthorPaths],
	FinalizeFunc[map[string]AuthorPaths, []Tally],
) {
	apply := func(commits iter.Seq2[git.Commit, error]) (
		map[string]AuthorPaths,
		error,
	) {
		return tallyByPaths(commits, wtreeset, opts)
	}

	merge := func(a, b map[string]AuthorPaths) map[string]AuthorPaths {
		union := b
		for key, aAuthor := range a {
			bAuthor, ok := b[key]
			if ok {
				aAuthor = aAuthor.Union(bAuthor)
			}
			union[key] = aAuthor
		}

		return union
	}

	finalize := func(authors map[string]AuthorPaths) []Tally {
		tallies := sumOverPaths(authors, wtreeset, allowOutsideWorktree)
		return sortTallies(tallies, opts.Mode)
	}

	return apply, merge, finalize
}

func tallyCommits(
	commits iter.Seq2[git.Commit, error],
	wtreefiles map[string]bool,
	allowOutsideWorktree bool,
	opts TallyOpts,
) (map[string]Tally, error) {
	// Map of author to final tally
	var authorTallies map[string]Tally

	start := time.Now()

	if !opts.IsDiffMode() && allowOutsideWorktree {
		authorTallies = map[string]Tally{}

		// Just sum over commits
		for commit, err := range commits {
			if err != nil {
				return nil, fmt.Errorf("error iterating commits: %w", err)
			}

			key := opts.Key(commit)

			authorTally := authorTallies[key]
			authorTally.AuthorName = commit.AuthorName
			authorTally.AuthorEmail = commit.AuthorEmail
			authorTally.Commits += 1
			authorTally.LastCommitTime = timeutils.Max(
				commit.Date,
				authorTally.LastCommitTime,
			)

			authorTallies[key] = authorTally
		}
	} else {
		pathTallies, err := tallyByPaths(commits, wtreefiles, opts)
		if err != nil {
			return nil, err
		}

		authorTallies = sumOverPaths(
			pathTallies,
			wtreefiles,
			allowOutsideWorktree,
		)
	}

	elapsed := time.Now().Sub(start)
	logger().Debug("tallied commits", "duration_ms", elapsed.Milliseconds())

	return authorTallies, nil
}

func sortTallies(tallies map[string]Tally, mode TallyMode) []Tally {
	sorted := slices.SortedFunc(maps.Values(tallies), func(a, b Tally) int {
		return -a.Compare(b, mode)
	})

	return sorted
}

func sumOverPaths(
	authors map[string]AuthorPaths,
	wtreefiles map[string]bool,
	allowOutsideWorktree bool,
) map[string]Tally {
	authorTallies := map[string]Tally{}

	for key, author := range authors {
		authorTally := authorTallies[key]
		authorTally.AuthorName = author.name
		authorTally.AuthorEmail = author.email

		runningTally := newTally(0)
		for path, pathTally := range author.paths {
			if inWTree := wtreefiles[path]; inWTree || allowOutsideWorktree {
				runningTally = runningTally.Add(pathTally)
			}
		}

		authorTally.Commits = runningTally.Commits()
		authorTally.LinesAdded = runningTally.added
		authorTally.LinesRemoved = runningTally.removed
		authorTally.FileCount = runningTally.numTallied
		authorTally.LastCommitTime = runningTally.lastCommitTime

		authorTallies[key] = authorTally
	}

	return authorTallies
}
