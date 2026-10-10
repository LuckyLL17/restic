package main

import (
	"context"
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/global"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/retention"
	"github.com/restic/restic/internal/ui"
	"github.com/restic/restic/internal/ui/progress"
)

func newRetainedCommand(globalOptions *global.Options) *cobra.Command {
	var opts RetainedOptions

	cmd := &cobra.Command{
		Use:   "retained [flags] ID...",
		Short: "Explain why packs or blobs are still retained",
		Long: `
The "retained" command looks up the given pack or blob IDs and explains why
they are still present in the repository. It reverse-traces snapshots, trees
and data references and distinguishes objects that are referenced by a
snapshot, objects only retained by the index, pack files that are already
unreferenced and can be reclaimed, and objects whose retention status cannot
be traced because repository data is missing.

The command is read-only: it never modifies the repository, the index or any
snapshot.

EXIT STATUS
===========

Exit status is 0 if the command was successful.
Exit status is 1 if there was any error.
Exit status is 10 if the repository does not exist.
Exit status is 11 if the repository is already locked.
Exit status is 12 if the password is incorrect.
`,
		Example: `restic retained 025c1d06
restic retained --blob 420f620f b46ebe8a
restic retained --pack --json 025c1d06`,
		GroupID:           cmdGroupDefault,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRetained(cmd.Context(), opts, *globalOptions, args)
		},
	}

	opts.AddFlags(cmd.Flags())
	return cmd
}

// RetainedOptions bundles all options for the retained command.
type RetainedOptions struct {
	PackID bool
	BlobID bool
}

func (opts *RetainedOptions) AddFlags(f *pflag.FlagSet) {
	f.BoolVar(&opts.PackID, "pack", false, "interpret IDs as pack IDs")
	f.BoolVar(&opts.BlobID, "blob", false, "interpret IDs as blob IDs")
}

func runRetained(ctx context.Context, opts RetainedOptions, gopts global.Options, args []string) error {
	if len(args) == 0 {
		return errors.Fatal("wrong number of arguments")
	}
	if opts.PackID && opts.BlobID {
		return errors.Fatal("--pack and --blob are mutually exclusive")
	}

	printer := progress.NewTerminalPrinter(gopts.JSON, gopts.Verbosity, gopts.Term)

	ctx, repo, unlock, err := openWithReadLock(ctx, gopts, gopts.NoLock, printer)
	if err != nil {
		return err
	}
	defer unlock()

	// list snapshots before loading the index, as required for eventually
	// consistent backends
	snapshotLister, err := restic.MemorizeList(ctx, repo, restic.SnapshotFile)
	if err != nil {
		return err
	}

	if err = repo.LoadIndex(ctx, printer); err != nil {
		return err
	}

	kind := retention.ObjectKind("")
	switch {
	case opts.PackID:
		kind = retention.KindPack
	case opts.BlobID:
		kind = retention.KindBlob
	}

	queries := make([]retention.Query, 0, len(args))
	for _, id := range args {
		queries = append(queries, retention.Query{Kind: kind, ID: id})
	}

	results, err := retention.Analyze(ctx, repo, snapshotLister, queries)
	if err != nil {
		return err
	}

	if gopts.JSON {
		return writeRetainedJSON(gopts, results)
	}
	printRetained(printer, results)
	return nil
}

func writeRetainedJSON(gopts global.Options, results []*retention.Result) error {
	type jsonReference struct {
		Snapshot      string   `json:"snapshot"`
		Time          string   `json:"time"`
		Hostname      string   `json:"hostname,omitempty"`
		SnapshotPaths []string `json:"snapshot_paths,omitempty"`
		BlobID        string   `json:"blob_id"`
		BlobType      string   `json:"blob_type"`
		Path          string   `json:"path"`
		ParentTree    string   `json:"parent_tree"`
		Chain         []string `json:"chain"`
	}
	type jsonLocation struct {
		BlobID      string `json:"blob_id"`
		BlobType    string `json:"blob_type"`
		PackID      string `json:"pack_id"`
		PackPresent bool   `json:"pack_present"`
	}
	type jsonResult struct {
		ObjectType   string          `json:"object_type"`
		ID           string          `json:"id"`
		Status       string          `json:"status"`
		Reason       string          `json:"reason,omitempty"`
		InBackend    bool            `json:"in_backend"`
		InIndex      bool            `json:"in_index"`
		PhysicalSize int64           `json:"physical_size,omitempty"`
		PackBlobs    []jsonLocation  `json:"pack_blobs,omitempty"`
		Locations    []jsonLocation  `json:"locations,omitempty"`
		References   []jsonReference `json:"references,omitempty"`
		Warnings     []string        `json:"warnings,omitempty"`
	}

	out := make([]jsonResult, 0, len(results))
	for _, r := range results {
		j := jsonResult{
			ObjectType:   string(r.Kind),
			ID:           r.ID.String(),
			Status:       string(r.Status),
			Reason:       r.Reason,
			InBackend:    r.InBackend,
			InIndex:      r.InIndex,
			PhysicalSize: r.PhysicalSize,
			Warnings:     r.Warnings,
		}
		for _, h := range r.PackBlobs {
			j.PackBlobs = append(j.PackBlobs, jsonLocation{BlobID: h.ID.String(), BlobType: h.Type.String()})
		}
		for _, l := range r.Locations {
			j.Locations = append(j.Locations, jsonLocation{
				BlobID:      l.BlobID.String(),
				BlobType:    l.BlobType.String(),
				PackID:      l.PackID.String(),
				PackPresent: l.PackPresent,
			})
		}
		for _, ref := range r.References {
			chain := make([]string, 0, len(ref.Chain))
			for _, id := range ref.Chain {
				chain = append(chain, id.String())
			}
			j.References = append(j.References, jsonReference{
				Snapshot:      ref.Snapshot.String(),
				Time:          ref.Time.UTC().Format(global.TimeFormat),
				Hostname:      ref.Hostname,
				SnapshotPaths: ref.SnapshotPaths,
				BlobID:        ref.BlobID.String(),
				BlobType:      ref.BlobType.String(),
				Path:          ref.Path,
				ParentTree:    ref.ParentTree.String(),
				Chain:         chain,
			})
		}
		out = append(out, j)
	}

	buf, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = gopts.Term.OutputRaw().Write(append(buf, '\n'))
	return err
}

func printRetained(printer restic.Printer, results []*retention.Result) {
	for _, r := range results {
		printer.S("%s %s", r.Kind, r.ID)
		printer.S("  state: %s", r.Status)
		printer.S("  %s", r.Reason)
		if r.Kind == retention.KindPack {
			printer.S("  pack file present: %v, listed in index: %v", r.InBackend, r.InIndex)
			if r.InBackend {
				printer.S("  physical size: %s", ui.FormatBytes(uint64(r.PhysicalSize)))
			}
			if len(r.PackBlobs) > 0 {
				printer.S("  blobs in pack: %d", len(r.PackBlobs))
			}
		} else if len(r.Locations) > 0 {
			for _, l := range r.Locations {
				state := "present"
				if !l.PackPresent {
					state = "MISSING"
				}
				printer.S("  %s blob %s stored in pack %s (%s)", l.BlobType, l.BlobID.Str(), l.PackID.Str(), state)
			}
		}

		if len(r.References) > 0 {
			printer.S("  references:")
			for _, ref := range r.References {
				printer.S("    %s blob %s", ref.BlobType, ref.BlobID.Str())
				printer.S("      snapshot %s (%s, %s)", ref.Snapshot.Str(), ref.Hostname, ref.Time.Local().Format(global.TimeFormat))
				printer.S("      path: %s", ref.Path)
				if len(ref.Chain) > 0 {
					printer.S("      tree chain: %s", formatIDChain(ref.Chain))
				}
			}
		}

		for _, w := range r.Warnings {
			printer.E("warning: %s", w)
		}
	}
}

func formatIDChain(ids restic.IDs) string {
	s := ""
	for i, id := range ids {
		if i > 0 {
			s += " -> "
		}
		s += id.Str()
	}
	return s
}
