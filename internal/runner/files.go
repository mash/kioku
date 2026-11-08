package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
)

const OutputArchiveName = "output.tar.gz"

func InputArchiveName(key AttemptKey) string { return "kioku-input-" + key.AttemptID + ".tar.gz" }

// StartFileSession journals the upload and session creation separately. A
// lost response is reconciled by name/metadata; a create is never replayed.
func StartFileSession(ctx context.Context, spec RunSpec, provider string,
	upload func(context.Context, string, []byte) (string, error),
	start func(context.Context, string, func(string) error) (string, error),
) (RunRef, error) {
	ref := RunRef{UsesProviderFiles: true, Resources: map[string]string{"provider": provider}}
	if spec.SaveRef == nil || len(spec.InputArchive) == 0 || spec.Key.AttemptID == "" ||
		spec.Key.JobID == "" || spec.Key.ProjectID == "" || spec.Token != "" ||
		spec.InputURL != "" || spec.OutputURL != "" {
		return ref, errors.New("invalid file runner input")
	}
	save := func() error {
		if err := spec.SaveRef(RunRef{ID: ref.ID, UsesProviderFiles: ref.UsesProviderFiles, Resources: maps.Clone(ref.Resources)}); err != nil {
			return fmt.Errorf("%w: could not record file runner resources", ErrTemporary)
		}
		return nil
	}
	ref.Resources["upload_started"] = "true"
	if err := save(); err != nil {
		return ref, err
	}
	id, err := upload(ctx, InputArchiveName(spec.Key), spec.InputArchive)
	if err != nil {
		return ref, err
	}
	if id == "" {
		return ref, fmt.Errorf("%w: missing input file ID", ErrTemporary)
	}
	ref.Resources["input_file_id"] = id
	if err = save(); err != nil {
		return ref, err
	}
	persistSession := func(id string) error {
		if id == "" {
			return fmt.Errorf("%w: missing session ID", ErrTemporary)
		}
		ref.ID = id
		return save()
	}
	session, err := start(ctx, id, persistSession)
	if err != nil {
		return ref, err
	}
	if ref.ID != session || ref.ID == "" {
		err = persistSession(session)
	}
	return ref, err
}

func ReadOutputArchive(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: could not read output archive", ErrTemporary)
	}
	if int64(len(body)) > limit {
		return nil, errors.New("managed output archive exceeds the compressed size limit")
	}
	return body, nil
}
