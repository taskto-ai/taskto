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

const (
	transcribeTimeout  = 5 * time.Minute
	apiTimeout         = 4 * time.Minute
	egressCheckTimeout = 10 * time.Second
	uploadTimeout      = 2 * time.Minute
)

const maxInFlight = 4

const maxPerUser = 1

type Config struct {
	DB      *sql.DB
	Key     *serverkey.Key
	Docs    Docs
	Egress  egress.Policy
	Local   Local
	Addons  []Installed
	Scratch string
}

type Service struct {
	db         *sql.DB
	key        *serverkey.Key
	docs       Docs
	egress     egress.Policy
	client     *http.Client
	local      Local
	addons     map[string]*addonEngine
	busy       chan struct{}
	mu         sync.Mutex
	settingsMu sync.Mutex
	users      map[int64]int
	resolve    egress.Resolver
	language   func(r *http.Request, browser string) string
}

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

func (s *Service) checkEgress(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, egressCheckTimeout)
	defer cancel()
	return s.egress.Check(ctx, host, s.resolve)
}

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
