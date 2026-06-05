package githubstats

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	defaultCacheTTL = 12 * time.Hour
	githubAPIBase   = "https://api.github.com"
)

var trackedRepos = []string{
	"zentra-chat/web",
	"zentra-chat/desktop",
	"zentra-chat/server",
	"zentra-chat/plugin-template",
	"zentra-chat/plugin-sdk",
	"zentra-chat/plugin-marketplace",
	"zentra-chat/docs",
}

type Contributor struct {
	Login         string `json:"login"`
	AvatarURL     string `json:"avatar_url"`
	HTMLURL       string `json:"html_url"`
	Contributions int    `json:"contributions"`
}

type Stats struct {
	Stars        int           `json:"stars"`
	Forks        int           `json:"forks"`
	Contributors []Contributor `json:"contributors"`
	UpdatedAt    time.Time     `json:"updatedAt"`
}

type repoSummary struct {
	StargazersCount int `json:"stargazers_count"`
	ForksCount      int `json:"forks_count"`
}

type repoContributor struct {
	Login         string `json:"login"`
	AvatarURL     string `json:"avatar_url"`
	HTMLURL       string `json:"html_url"`
	Contributions int    `json:"contributions"`
}

type Service struct {
	httpClient *http.Client
	token      string
	cacheTTL   time.Duration

	mu    sync.RWMutex
	cache *Stats
}

func NewService(token string) *Service {
	return &Service{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		token:      strings.TrimSpace(token),
		cacheTTL:   defaultCacheTTL,
	}
}

func (s *Service) GetStats(ctx context.Context) (*Stats, error) {
	if cached := s.getCachedIfFresh(); cached != nil {
		return cached, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil && time.Since(s.cache.UpdatedAt) < s.cacheTTL {
		copy := *s.cache
		return &copy, nil
	}

	fresh, err := s.fetchStats(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch GitHub stats, falling back to cache")
		if s.cache != nil {
			copy := *s.cache
			return &copy, nil
		}
		return nil, err
	}

	s.cache = fresh
	copy := *fresh
	return &copy, nil
}

func (s *Service) getCachedIfFresh() *Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.cache == nil || time.Since(s.cache.UpdatedAt) >= s.cacheTTL {
		return nil
	}

	copy := *s.cache
	return &copy
}

func (s *Service) fetchStats(ctx context.Context) (*Stats, error) {
	totalStars := 0
	totalForks := 0
	contributorMap := make(map[string]*Contributor)

	for _, repo := range trackedRepos {
		summary, err := s.fetchRepoSummary(ctx, repo)
		if err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("Failed to fetch GitHub repo summary")
			return nil, err
		}
		totalStars += summary.StargazersCount
		totalForks += summary.ForksCount

		repoContributors, err := s.fetchRepoContributors(ctx, repo)
		if err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("Failed to fetch GitHub repo contributors")
			return nil, err
		}

		for _, c := range repoContributors {
			if c.Login == "" {
				continue
			}

			existing, ok := contributorMap[c.Login]
			if !ok {
				contributorMap[c.Login] = &Contributor{
					Login:         c.Login,
					AvatarURL:     c.AvatarURL,
					HTMLURL:       c.HTMLURL,
					Contributions: c.Contributions,
				}
				continue
			}

			existing.Contributions += c.Contributions
			if existing.AvatarURL == "" {
				existing.AvatarURL = c.AvatarURL
			}
			if existing.HTMLURL == "" {
				existing.HTMLURL = c.HTMLURL
			}
		}
	}

	contributors := make([]Contributor, 0, len(contributorMap))
	for _, c := range contributorMap {
		contributors = append(contributors, *c)
	}

	sort.Slice(contributors, func(i, j int) bool {
		if contributors[i].Contributions == contributors[j].Contributions {
			return contributors[i].Login < contributors[j].Login
		}
		return contributors[i].Contributions > contributors[j].Contributions
	})

	return &Stats{
		Stars:        totalStars,
		Forks:        totalForks,
		Contributors: contributors,
		UpdatedAt:    time.Now().UTC(),
	}, nil
}

func (s *Service) fetchRepoSummary(ctx context.Context, repo string) (*repoSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s", githubAPIBase, repo), nil)
	if err != nil {
		return nil, err
	}
	s.applyGitHubHeaders(req)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("github summary request failed for %s: %s", repo, resp.Status)
		log.Warn().Err(err).Str("repo", repo).Int("status", resp.StatusCode).Msg("GitHub API returned non-2xx for repo summary")
		return nil, err
	}

	var summary repoSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return nil, err
	}

	return &summary, nil
}

func (s *Service) fetchRepoContributors(ctx context.Context, repo string) ([]repoContributor, error) {
	all := make([]repoContributor, 0)
	page := 1

	for {
		url := fmt.Sprintf("%s/repos/%s/contributors?per_page=100&page=%d", githubAPIBase, repo, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		s.applyGitHubHeaders(req)

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			err := fmt.Errorf("github contributors request failed for %s: %s", repo, resp.Status)
			log.Warn().Err(err).Str("repo", repo).Int("status", resp.StatusCode).Int("page", page).Msg("GitHub API returned non-2xx for repo contributors")
			return nil, err
		}

		var pageContributors []repoContributor
		err = json.NewDecoder(resp.Body).Decode(&pageContributors)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		if len(pageContributors) == 0 {
			break
		}

		all = append(all, pageContributors...)
		if len(pageContributors) < 100 {
			break
		}

		page++
	}

	return all, nil
}

func (s *Service) applyGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "zentra-server-github-stats")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
}
