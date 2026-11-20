package faucet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"
	"sync"

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

// Validate reports whether a store may keep o.
func (o Owner) Validate() error {
	switch {
	case o.ID <= 0:
		return xerrors.New("an owner needs a positive ID")
	case o.Login == "":
		return xerrors.New("an owner needs a login")
	case strings.ContainsAny(o.Login, "/\x00"):
		return xerrors.New("an owner's login holds no slash and no NUL byte")
	}
	return nil
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

// RepositoryRename is a repository event that renamed a repository or
// transferred it to another owner. Repo holds its names after the event, and
// From its full name before, as GitHub wrote it.
type RepositoryRename struct {
	Repo Repository
	From string
	// Body is the SHA-256 of the delivery's signed body. GitHub signs the
	// body alone, so a replay of it under another delivery GUID has the same
	// Body.
	Body [sha256.Size]byte
	// Late is true for a delivery that faucet's redelivery asked GitHub to
	// send again. It can come after a newer rename of the repository.
	Late bool
}

// RenameOutcome is what Store.RenameRepository did with a rename.
type RenameOutcome int

const (
	// Renamed means the repository took the rename's names: the store held
	// the name that it renamed the repository from, or no row of it.
	Renamed RenameOutcome = iota + 1
	// RenameQuarantined means the store held another name: a rename before
	// this one never reached it, or this one is an old body sent again, and
	// the store cannot tell which. The repository resolves no name until a
	// fresh rename from the name that the store holds.
	RenameQuarantined
	// RenameLate means the rename was late and the store held another name,
	// which a newer rename can have given, so it changed nothing.
	RenameLate
	// RenameSeenBefore means the store had judged a rename with the same body
	// before, so it changed nothing.
	RenameSeenBefore
)

// recordRepository records the repository and the organization that a
// signed delivery names, so a service can be granted them by name before
// faucet stores an event of them, and so the login of each account follows
// its latest delivery. A delivery that redelivery asked GitHub to send again
// names the accounts as they were when it first came, so it adds an account
// that the store lacks and moves no login. Only a repository event that
// renames or transfers a repository changes the names of a repository that
// the store knows (Store.RenameRepository). A failed write of such an event
// fails the delivery, so that GitHub records the failure and redelivery asks
// for it again. Any other failed write is logged, and the delivery goes on:
// the next one that names the accounts writes them.
func (s *Service) recordRepository(ctx context.Context, logger slog.Logger, eventName, deliveryID string, payload []byte) error {
	named, hasRepo := repositoryOf(eventName, payload)
	var owners []Owner
	if org, ok := organizationOf(payload); ok {
		owners = append(owners, org)
	}
	if hasRepo && !slices.ContainsFunc(owners, func(o Owner) bool { return o.ID == named.repo.OwnerID }) {
		owners = append(owners, Owner{ID: named.repo.OwnerID, Login: named.repo.OwnerLogin})
	}
	replay := sync.OnceValue(func() bool { return s.replayed(ctx, logger, deliveryID) })
	for _, owner := range owners {
		s.recordOwner(ctx, logger, owner, replay)
	}
	switch {
	case !hasRepo:
		return nil
	case named.previous == "":
		if err := s.store.RecordRepository(ctx, named.repo, s.clock.Now()); err != nil {
			logger.Warn(ctx, "could not record the repository", slog.Error(err))
		}
		return nil
	}
	rename := RepositoryRename{Repo: named.repo, From: named.previous, Body: sha256.Sum256(payload), Late: replay()}
	outcome, err := s.store.RenameRepository(ctx, rename, s.clock.Now())
	if err != nil {
		return xerrors.Errorf("rename the repository: %w", err)
	}
	if outcome == RenameQuarantined {
		logger.Warn(ctx, "a rename or transfer of the repository never reached faucet, or an old one came again, so the repository resolves no name",
			slog.F("repo_id", rename.Repo.ID), slog.F("from", rename.From), slog.F("full_name", rename.Repo.FullName))
	}
	return nil
}

// RedeliveryWants is the value that a host gives redelivery.Deps.Wants. It
// wants what RawWants wants and, when the service records repositories, every
// repository event that renamed or transferred a repository, so that
// redelivery asks again for one whose write failed.
func (s *Service) RedeliveryWants(ctx context.Context) (func(event, action string, repoID int64) bool, error) {
	raw, err := s.RawWants(ctx)
	return func(event, action string, repoID int64) bool {
		return s.recordRepos && renamesRepository(event, action) || raw(event, action, repoID)
	}, err
}

// renamesRepository reports whether a delivery of the event and action
// renamed or transferred a repository.
func renamesRepository(eventName, action string) bool {
	return eventName == "repository" && (action == "renamed" || action == "transferred")
}

// recordOwner writes the login of owner when it is new or changed, which
// few deliveries find, so most write nothing and never ask whether they are
// a replay. A replay only adds an account that the store lacks.
func (s *Service) recordOwner(ctx context.Context, logger slog.Logger, owner Owner, replay func() bool) {
	held, err := s.store.OwnerByID(ctx, owner.ID)
	switch {
	case err == nil && held.Login == owner.Login:
		return
	case err != nil && !xerrors.Is(err, ErrUnknownOwner):
		logger.Warn(ctx, "could not read the owner", slog.Error(err))
		return
	case replay() && err == nil:
		return
	case replay():
		err = s.store.AddOwner(ctx, owner)
	default:
		err = s.store.RecordOwner(ctx, owner)
	}
	if err != nil {
		logger.Warn(ctx, "could not record the owner", slog.Error(err))
	}
}

// replayed reports whether redelivery asked GitHub to send the delivery of
// deliveryID again. When it cannot tell, it says yes, so that the delivery
// moves no login, and a rename moves a name only from the name that the store
// holds: a name that stays stale until the next delivery is safer than one
// that a replay gives back.
func (s *Service) replayed(ctx context.Context, logger slog.Logger, deliveryID string) bool {
	replay, err := s.redelivered(ctx, deliveryID)
	if err != nil {
		logger.Warn(ctx, "could not tell whether the delivery is a redelivery, so it is taken as one", slog.Error(err))
		return true
	}
	return replay
}

// organizationOf returns the organization that a delivery's payload names,
// as an organization's own events and the events of its repositories do,
// and false when it names none or names it in a form that Validate refuses.
func organizationOf(payload []byte) (Owner, bool) {
	var body struct {
		Organization *struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Organization == nil {
		return Owner{}, false
	}
	org := Owner{ID: body.Organization.ID, Login: body.Organization.Login}
	return org, org.Validate() == nil
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
	if !renamesRepository(eventName, body.Action) {
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
