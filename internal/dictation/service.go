package dictation

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/serverkey"
)

// Timeouts: a transcription, waiting for the local engine included, and a call to a hosted API.
const (
	transcribeTimeout  = 5 * time.Minute
	apiTimeout         = 4 * time.Minute
	egressCheckTimeout = 10 * time.Second
	// uploadTimeout bounds reading a dictation's upload: 26 MiB at 2 Mbit/s.
	uploadTimeout = 2 * time.Minute
)

// maxInFlight is how many transcriptions the server takes at once. Each holds its audio:
// up to about 70 MB while its upload is decoded, then about 40 MB (the samples, and the WAV
// an engine is given), so this bounds their memory to about 300 MB.
const maxInFlight = 4

// maxPerUser is how many of those one person may hold: the web app sends one at a time.
const maxPerUser = 1

// Config is what a Service is made of.
type Config struct {
	DB  *sql.DB
	Key *serverkey.Key
	// Docs is the knowledge base, for the glossary.
	Docs Docs
	// Egress is the policy of workspaces' provider accounts: the API engine's endpoint
	// follows it, when it is saved and on every connection.
	Egress egress.Policy
	// Local is the operator's local engine; nil when it is off.
	Local Local
	// Addons are the add-ons the operator installed (DetectAddons).
	Addons []Installed
	// Scratch is where add-ons get a folder of their own for each run.
	Scratch string
}

// Service serves the dictation API (docs/api.md#dictation).
type Service struct {
	db         *sql.DB
	key        *serverkey.Key
	docs       Docs
	egress     egress.Policy
	client     *http.Client
	local      Local
	addons     map[string]*addonEngine
	busy       chan struct{} // a slot per transcription in flight
	mu         sync.Mutex
	settingsMu sync.Mutex      // one settings change at a time
	users      map[int64]int   // transcriptions in flight per user
	resolve    egress.Resolver // nil: the system's
	// language is the caller's language by default (auth.Service.Language, set by Routes).
	language func(r *http.Request, browser string) string
}

// New returns the service of o.
func New(o Config) *Service {
	s := &Service{
		db: o.DB, key: o.Key, docs: o.Docs, egress: o.Egress, local: o.Local,
		client: o.Egress.Client(apiTimeout), busy: make(chan struct{}, maxInFlight),
		users: map[int64]int{}, addons: map[string]*addonEngine{},
	}
	for _, a := range o.Addons {
		if a.Kind == KindSpeechToText {
			s.addons[a.ID] = &addonEngine{Installed: a, scratch: o.Scratch, jobs: make(chan struct{}, 1)}
		}
	}
	return s
}

// engine is the engine st uses now, its name, or an *Unavailable saying why there is none.
func (s *Service) engine(workspaceID int64, st settings) (string, Engine, error) {
	name := st.engine
	switch name {
	case EngineAPI:
		key, err := s.apiKey(workspaceID, st)
		if err != nil {
			return "", nil, err
		}
		return name, apiEngine{client: s.client, baseURL: st.baseURL, model: st.model, key: key}, nil
	case EngineLocal:
		if s.local == nil {
			return "", nil, &Unavailable{
				Reason: "the server's local speech model is off (dictation_local): a workspace admin can choose another engine in Settings",
				Code:   apierr.CodeDictationLocalOff,
			}
		}
		return name, s.local, nil
	}
	if a, ok := s.addons[name]; ok {
		return name, a, nil
	}
	if a, ok := addonByID(name); ok {
		return "", nil, &Unavailable{
			Reason: fmt.Sprintf("the %s add-on (%s) isn't installed on this server any more", a.Name, a.Vendor),
			Code:   apierr.CodeDictationAddonMissing, Params: apierr.Params{"name": a.Name, "vendor": a.Vendor},
		}
	}
	return "", nil, &Unavailable{
		Reason: "this workspace's dictation engine isn't available on this server",
		Code:   apierr.CodeDictationEngineUnavailable,
	}
}

// checkEgress refuses a base URL the egress policy doesn't allow, resolving its host now; a
// host that doesn't resolve now is left to the check of every connection.
func (s *Service) checkEgress(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, egressCheckTimeout)
	defer cancel()
	return s.egress.Check(ctx, host, s.resolve)
}

// claim takes one of userID's slots, if one is free.
func (s *Service) claim(userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[userID] >= maxPerUser {
		return false
	}
	s.users[userID]++
	return true
}

func (s *Service) release(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[userID]--; s.users[userID] <= 0 {
		delete(s.users, userID)
	}
}
