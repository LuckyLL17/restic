package main

import (
	"context"
	"encoding/json"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/differ"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/global"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/ui"
	"github.com/restic/restic/internal/ui/progress"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newDiffCommand(globalOptions *global.Options) *cobra.Command {
	var opts DiffOptions

	cmd := &cobra.Command{
		Use:   "diff [flags] snapshotID snapshotID",
		Short: "Show differences between two snapshots",
		Long: `
The "diff" command shows differences from the first to the second snapshot. The
first characters in each line display what has happened to a particular file or
directory:

* +  The item was added
* -  The item was removed
* U  The metadata (access mode, timestamps, ...) for the item was updated
* M  The file's content was modified
* T  The type was changed, e.g. a file was made a symlink
* ?  Bitrot detected: The file's content has changed but all metadata is the same

Metadata comparison will likely not work if a backup was created using the
'--ignore-inode' or '--ignore-ctime' option.

To only compare files in specific subfolders, you can use the
"snapshotID:subfolder" syntax, where "subfolder" is a path within the
snapshot tree as shown by "restic ls".

EXIT STATUS
===========

Exit status is 0 if the command was successful.
Exit status is 1 if there was any error.
Exit status is 10 if the repository does not exist.
Exit status is 11 if the repository is already locked.
Exit status is 12 if the password is incorrect.
`,
		GroupID:           cmdGroupDefault,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd.Context(), opts, *globalOptions, args, globalOptions.Term)
		},
	}

	opts.AddFlags(cmd.Flags())
	return cmd
}

// DiffOptions collects all options for the diff command.
type DiffOptions differ.Options

func (opts *DiffOptions) AddFlags(f *pflag.FlagSet) {
	f.BoolVar(&opts.ShowMetadata, "metadata", false, "print changes in metadata")
}

func loadSnapshot(ctx context.Context, be restic.Lister, repo restic.LoaderUnpacked, desc string) (*data.Snapshot, string, error) {
	sn, subfolder, err := data.FindSnapshot(ctx, be, repo, desc)
	if err != nil {
		return nil, "", errors.Fatalf("%s", err)
	}
	return sn, subfolder, err
}

// The diff command and other in-repository callers (such as audit tasks) share
// the comparison semantics in internal/differ.
type (
	Change             = differ.Change
	DiffStat           = differ.Stat
	DiffStatsContainer = differ.Stats
)

func runDiff(ctx context.Context, opts DiffOptions, gopts global.Options, args []string, term ui.Terminal) error {
	if len(args) != 2 {
		return errors.Fatalf("specify two snapshot IDs")
	}

	printer := progress.NewTerminalPrinter(gopts.JSON, gopts.Verbosity, term)

	ctx, repo, unlock, err := openWithReadLock(ctx, gopts, gopts.NoLock, printer)
	if err != nil {
		return err
	}
	defer unlock()

	// cache snapshots listing
	be, err := restic.MemorizeList(ctx, repo, restic.SnapshotFile)
	if err != nil {
		return err
	}
	sn1, subfolder1, err := loadSnapshot(ctx, be, repo, args[0])
	if err != nil {
		return err
	}

	sn2, subfolder2, err := loadSnapshot(ctx, be, repo, args[1])
	if err != nil {
		return err
	}

	if !gopts.JSON {
		printer.P("comparing snapshot %v to %v:\n\n", sn1.ID().Str(), sn2.ID().Str())
	}
	if err = repo.LoadIndex(ctx, printer); err != nil {
		return err
	}

	if sn1.Tree == nil {
		return errors.Errorf("snapshot %v has nil tree", sn1.ID().Str())
	}

	if sn2.Tree == nil {
		return errors.Errorf("snapshot %v has nil tree", sn2.ID().Str())
	}

	sn1.Tree, err = data.FindTreeDirectory(ctx, repo, sn1.Tree, subfolder1)
	if err != nil {
		return err
	}

	sn2.Tree, err = data.FindTreeDirectory(ctx, repo, sn2.Tree, subfolder2)
	if err != nil {
		return err
	}

	c := differ.New(repo, differ.Options(opts))

	printChange := differ.ChangeReporter(func(change *Change) {
		printer.S("%-5s%v", change.Modifier, change.Path)
	})

	if gopts.JSON {
		enc := json.NewEncoder(gopts.Term.OutputWriter())
		printChange = func(change *Change) {
			err := enc.Encode(change)
			if err != nil {
				printer.E("JSON encode failed: %v", err)
			}
		}
	}

	if gopts.Quiet {
		printChange = func(_ *Change) {}
	}

	stats, err := c.Compare(ctx, *sn1.Tree, *sn2.Tree, printChange, printer.E)
	if err != nil {
		return err
	}
	c.Finalize(stats, printer.E)

	stats.SourceSnapshot = args[0]
	stats.TargetSnapshot = args[1]

	if gopts.JSON {
		err := json.NewEncoder(gopts.Term.OutputWriter()).Encode(stats)
		if err != nil {
			printer.E("JSON encode failed: %v", err)
		}
	} else {
		printer.S("")
		printer.S("Files:       %5d new, %5d removed, %5d changed", stats.Added.Files, stats.Removed.Files, stats.ChangedFiles)
		printer.S("Dirs:        %5d new, %5d removed", stats.Added.Dirs, stats.Removed.Dirs)
		printer.S("Others:      %5d new, %5d removed", stats.Added.Others, stats.Removed.Others)
		printer.S("Data Blobs:  %5d new, %5d removed", stats.Added.DataBlobs, stats.Removed.DataBlobs)
		printer.S("Tree Blobs:  %5d new, %5d removed", stats.Added.TreeBlobs, stats.Removed.TreeBlobs)
		printer.S("  Added:   %-5s", ui.FormatBytes(stats.Added.Bytes))
		printer.S("  Removed: %-5s", ui.FormatBytes(stats.Removed.Bytes))
	}

	return nil
}
