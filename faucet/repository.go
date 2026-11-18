package faucet

import (
	"context"
	"encoding/json"
	"strings"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// ErrUnknownRepository means the store has seen no delivery that names the
// repository.
var ErrUnknownRepository = xerrors.New("no delivery has named the repository")

// ErrUnknownOwner means the store has seen no delivery that names a
// repository of the owner.
var ErrUnknownOwner = xerrors.New("no delivery has named a repository of the owner")

// Owner is the user or organization that owns a repository, as the signed
// delivery that the store saw latest names it. GitHub keeps ID when the
// account is renamed, and Login can later name another account.
type Owner struct {
	ID    int64
	Login string
}

// Repository is a GitHub repository as a signed delivery names it. GitHub
// keeps ID when the repository is renamed or moves to another owner, and
// FullName, written owner/name, can later name another repository.
type Repository struct {
	ID         int64
	OwnerID    int64
	OwnerLogin string
	FullName   string
}

// Validate reports whether a store may keep r.
func (r Repository) Validate() error {
	owner, name, ok := strings.Cut(r.FullName, "/")
	switch {
	case r.ID <= 0 || r.OwnerID <= 0:
		return xerrors.New("a repository needs a positive ID and owner ID")
	case r.OwnerLogin == "":
		return xerrors.New("a repository needs its owner's login")
	case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
		return xerrors.New("a repository's full name is written owner/name")
	case strings.ContainsRune(r.FullName+r.OwnerLogin, 0):
		return xerrors.New("a repository's names hold no NUL byte")
	}
	return nil
}

// recordRepository records the repository that a signed delivery names, so a
// service can be granted it by name before faucet stores an event of it.
// Only a repository event that renames or transfers it, from the name that
// the store holds, changes the names of a repository the store knows. A
// failed write is logged, and the delivery goes on: the next one that names
// the repository writes it.
func (s *Service) recordRepository(ctx context.Context, logger slog.Logger, eventName string, payload []byte) {
	named, ok := repositoryOf(eventName, payload)
	if !ok {
		return
	}
	var err error
	if named.previous != "" {
		err = s.store.RenameRepository(ctx, named.repo, named.previous, s.clock.Now())
	} else {
		err = s.store.RecordRepository(ctx, named.repo, s.clock.Now())
	}
	if err != nil {
		logger.Warn(ctx, "could not record the repository", slog.Error(err))
	}
}

// namedRepository is the repository that a delivery names, and the full
// name that it had before when the delivery renamed or transferred it.
type namedRepository struct {
	repo     Repository
	previous string
}

// repositoryOf returns the repository that a delivery's payload names, and
// false when it names none or names it in a form that Validate refuses. The
// previous name comes from the changes of a repository event that renamed
// it (changes.repository.name.from) or transferred it (changes.owner.from),
// and is empty for any other delivery.
func repositoryOf(eventName string, payload []byte) (namedRepository, bool) {
	type login struct {
		Login string `json:"login"`
	}
	var body struct {
		Action     string `json:"action"`
		Repository *struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
			Owner    struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Changes struct {
			Repository struct {
				Name struct {
					From string `json:"from"`
				} `json:"name"`
			} `json:"repository"`
			Owner struct {
				From struct {
					User         *login `json:"user"`
					Organization *login `json:"organization"`
				} `json:"from"`
			} `json:"owner"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Repository == nil {
		return namedRepository{}, false
	}
	r := Repository{ID: body.Repository.ID, OwnerID: body.Repository.Owner.ID, OwnerLogin: body.Repository.Owner.Login, FullName: body.Repository.FullName}
	if r.Validate() != nil {
		return namedRepository{}, false
	}
	named := namedRepository{repo: r}
	if eventName != "repository" {
		return named, true
	}
	owner, name, _ := strings.Cut(r.FullName, "/")
	switch from := body.Changes.Owner.From; {
	case body.Action == "renamed" && body.Changes.Repository.Name.From != "":
		named.previous = owner + "/" + body.Changes.Repository.Name.From
	case body.Action == "transferred" && from.Organization != nil && from.Organization.Login != "":
		named.previous = from.Organization.Login + "/" + name
	case body.Action == "transferred" && from.User != nil && from.User.Login != "":
		named.previous = from.User.Login + "/" + name
	}
	return named, true
}
