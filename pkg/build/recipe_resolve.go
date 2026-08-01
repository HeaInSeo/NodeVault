package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// anacondaAPIBase is a var (not const) so tests can point it at a fake/closed
// server to simulate a real network failure — see recipe_resolve_test.go.
var anacondaAPIBase = "https://api.anaconda.org/package"

const channelCacheTTL = 30 * time.Minute

// channelCacheKey identifies a cached resolution: "channel/name/version".
type channelCacheEntry struct {
	candidates []*nfv1.BuildStringCandidate
	expiresAt  time.Time
}

var channelCache sync.Map

// ResolveRecipe implements BuildServiceServer.
// Pre-query for conda/micromamba/package_mirror/BioContainer variants:
// checks Harbor (via index) first, then falls back to an external source query.
// Closed-network callers receive InvalidArgument if the image is not in Harbor.
func (s *Service) ResolveRecipe(
	ctx context.Context, req *nfv1.ResolveRecipeRequest,
) (*nfv1.ResolveRecipeResponse, error) {
	if req.GetToolName() == "" || req.GetVersion() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_name and version are required")
	}
	if len(req.GetPackages()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one package is required")
	}

	// Harbor-first: check catalog for a previously built image with this tool+version.
	if s.registry != nil {
		stableRef := req.GetToolName() + "@" + req.GetVersion()
		listResp, listErr := s.registry.ListTools(ctx, &nfv1.ListToolsRequest{StableRef: stableRef})
		if listErr == nil && listResp != nil && len(listResp.GetTools()) > 0 {
			resolutions := extractResolutionsFromDefinitions(listResp.GetTools(), req.GetPackages())
			if len(resolutions) > 0 {
				return &nfv1.ResolveRecipeResponse{
					ResolutionSource: "harbor_cache",
					Packages:         resolutions,
				}, nil
			}
		}
	}

	// Harbor miss.
	if req.GetClosedNetwork() {
		return nil, status.Errorf(codes.InvalidArgument,
			"폐쇄망 환경에서 Harbor에 '%s@%s' 이미지가 없습니다. Harbor에 사전 등록 후 재시도하세요",
			req.GetToolName(), req.GetVersion())
	}

	// External source query.
	switch req.GetRecipeKind() {
	case nfv1.RecipeKind_RECIPE_KIND_CONDA,
		nfv1.RecipeKind_RECIPE_KIND_MICROMAMBA:
		return resolveCondaPackages(ctx, req)
	case nfv1.RecipeKind_RECIPE_KIND_PACKAGE_MIRROR:
		return resolvePackageMirror(ctx, req)
	case nfv1.RecipeKind_RECIPE_KIND_BIOCONTAINER:
		return nil, status.Error(codes.Unimplemented,
			"BioContainer external resolution is not yet supported (P3)")
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"unsupported variant: %v", req.GetRecipeKind())
	}
}

// extractResolutionsFromDefinitions parses EnvironmentSpec from existing tool definitions
// to find pinned build strings for the requested packages.
// Returns nil when no build strings can be extracted (e.g. no Active entries).
func extractResolutionsFromDefinitions(
	defs []*nfv1.RegisteredToolDefinition, packages []*nfv1.PackageSpec,
) []*nfv1.PackageResolution {
	var resolutions []*nfv1.PackageResolution

	for _, pkg := range packages {
		if pkg == nil {
			continue
		}
		buildStr := ""
		channel := ""
		for _, d := range defs {
			if d.GetLifecyclePhase() != string(index.PhaseActive) {
				continue
			}
			bs, ch := extractBuildString(d.GetEnvironmentSpec(), pkg.GetName(), pkg.GetVersion())
			if bs != "" {
				buildStr = bs
				channel = ch
				break
			}
		}
		if buildStr == "" {
			// No pinned build string found; skip this package.
			continue
		}
		resolutions = append(resolutions, &nfv1.PackageResolution{
			Name:    pkg.GetName(),
			Version: pkg.GetVersion(),
			Candidates: []*nfv1.BuildStringCandidate{
				{
					BuildString: buildStr,
					FullPin:     pkg.GetName() + "=" + pkg.GetVersion() + "=" + buildStr,
					Channel:     channel,
				},
			},
		})
	}
	return resolutions
}

// extractBuildString scans spec for a "name=version=build" pattern and returns
// (buildString, channel). channel is empty if not determinable from the spec.
func extractBuildString(spec, name, version string) (buildStr, channel string) {
	if spec == "" {
		return "", ""
	}
	prefix := name + "=" + version + "="
	for _, rawLine := range strings.Split(spec, "\n") {
		line := strings.TrimSpace(rawLine)
		// Strip leading "- " (conda YAML list item).
		line = strings.TrimPrefix(line, "- ")
		// Strip inline conda install args (e.g. "conda install -c bioconda bwa=0.7.17=h5bf99c6_8").
		if idx := strings.Index(line, prefix); idx >= 0 {
			rest := line[idx+len(prefix):]
			// build string ends at whitespace or end of line.
			if end := strings.IndexAny(rest, " \t"); end >= 0 {
				rest = rest[:end]
			}
			if rest != "" {
				// Try to extract channel from "-c channel" or "::channel" prefix pattern.
				ch := extractChannelFromLine(rawLine)
				return rest, ch
			}
		}
	}
	return "", ""
}

// extractChannelFromLine reads "-c <channel>" or "<channel>::" patterns from a conda RUN line.
func extractChannelFromLine(line string) string {
	tokens := strings.Fields(line)
	for i, tok := range tokens {
		if (tok == "-c" || tok == "--channel") && i+1 < len(tokens) {
			return tokens[i+1]
		}
		// channel::package form
		if idx := strings.Index(tok, "::"); idx >= 0 {
			return tok[:idx]
		}
	}
	return ""
}

// resolveCondaPackages queries the Anaconda.org package API for each requested package.
func resolveCondaPackages(
	ctx context.Context, req *nfv1.ResolveRecipeRequest,
) (*nfv1.ResolveRecipeResponse, error) {
	channels := req.GetChannels()
	if len(channels) == 0 {
		channels = []string{"bioconda", "conda-forge", "defaults"}
	}

	var resolutions []*nfv1.PackageResolution
	for _, pkg := range req.GetPackages() {
		if pkg == nil {
			continue
		}
		candidates, allChannelsUnreachable := queryAnacondaOrg(ctx, pkg.GetName(), pkg.GetVersion(), channels)
		if len(candidates) == 0 && allChannelsUnreachable {
			// 채널이 없어서(404) 못 찾은 게 아니라, 시도한 채널 전부에 실제로
			// 연결이 안 된 경우다. 이걸 "후보 0개"로 조용히 반환하면 클라이언트
			// 입장에서 "그 버전이 진짜 없다"와 구분이 안 된다 — 명확한 에러로
			// 알린다.
			return nil, status.Errorf(codes.Unavailable,
				"패키지 '%s=%s' 조회에 필요한 외부 conda 채널(%s)에 전부 연결할 수 없습니다. 네트워크 상태를 확인하세요",
				pkg.GetName(), pkg.GetVersion(), strings.Join(channels, ", "))
		}
		resolutions = append(resolutions, &nfv1.PackageResolution{
			Name:       pkg.GetName(),
			Version:    pkg.GetVersion(),
			Candidates: candidates,
		})
	}

	src := "external_source"
	if len(resolutions) == 0 {
		src = "not_found"
	}
	return &nfv1.ResolveRecipeResponse{
		ResolutionSource: src,
		Packages:         resolutions,
	}, nil
}

// resolvePackageMirror handles the package_mirror variant.
// The mirror URI is operator-managed, so no external query is attempted.
// Without Harbor cache, closed-network callers already received InvalidArgument above.
// In open-network, return not_found and let the caller handle it (the mirror admin
// must pre-register the package).
func resolvePackageMirror(
	_ context.Context, req *nfv1.ResolveRecipeRequest,
) (*nfv1.ResolveRecipeResponse, error) {
	if req.GetPackageMirrorUri() == "" {
		return nil, status.Error(codes.InvalidArgument, "package_mirror_uri is required for PACKAGE_MIRROR variant")
	}
	return &nfv1.ResolveRecipeResponse{
		ResolutionSource: "not_found",
		Packages:         nil,
	}, nil
}

// ── Anaconda.org API ──────────────────────────────────────────────────────────

// anacondaFile represents one file entry in the Anaconda.org package response.
type anacondaFile struct {
	Version string `json:"version"`
	Build   string `json:"build"`
	// Some API versions nest build inside attrs.
	Attrs struct {
		Build string `json:"build"`
	} `json:"attrs"`
	// Platform filtering: accept linux-64 / linux / x86_64 or empty.
	Platform string `json:"platform"` // e.g. "linux-64" or "linux"
	Arch     string `json:"arch"`     // e.g. "x86_64" or "64"
}

type anacondaPackageResp struct {
	Name  string         `json:"name"`
	Files []anacondaFile `json:"files"`
}

// queryAnacondaOrg queries Anaconda.org for build string candidates, trying channels
// in order. Results are cached per (channel, name, version) for channelCacheTTL.
// The second return value is true only if every channel query failed with a real
// error (network/HTTP failure) rather than a clean "not found in this channel" —
// callers use this to distinguish "genuinely no build for this version" from
// "could not reach any channel to check".
func queryAnacondaOrg(
	ctx context.Context, name, version string, channels []string,
) ([]*nfv1.BuildStringCandidate, bool) {
	var all []*nfv1.BuildStringCandidate
	seen := map[string]bool{}
	failedChannels := 0

	for _, ch := range channels {
		cacheKey := fmt.Sprintf("%s/%s/%s", ch, name, version)

		// Cache hit?
		if v, ok := channelCache.Load(cacheKey); ok {
			entry := v.(*channelCacheEntry)
			if time.Now().Before(entry.expiresAt) {
				for _, c := range entry.candidates {
					if !seen[c.GetBuildString()] {
						seen[c.GetBuildString()] = true
						all = append(all, c)
					}
				}
				continue
			}
			channelCache.Delete(cacheKey)
		}

		candidates, err := fetchAnacondaChannel(ctx, name, version, ch)
		if err != nil {
			// Real failure (network/HTTP) for this channel — distinct from a
			// clean 404, which fetchAnacondaChannel reports as (nil, nil).
			failedChannels++
			continue
		}

		channelCache.Store(cacheKey, &channelCacheEntry{
			candidates: candidates,
			expiresAt:  time.Now().Add(channelCacheTTL),
		})

		for _, c := range candidates {
			if !seen[c.GetBuildString()] {
				seen[c.GetBuildString()] = true
				all = append(all, c)
			}
		}
	}

	allChannelsUnreachable := len(channels) > 0 && failedChannels == len(channels)
	return all, allChannelsUnreachable
}

func fetchAnacondaChannel(
	ctx context.Context, name, version, channel string,
) ([]*nfv1.BuildStringCandidate, error) {
	url := fmt.Sprintf("%s/%s/%s", anacondaAPIBase, channel, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024)) // 2 MB cap
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var pkg anacondaPackageResp
	if err := json.Unmarshal(body, &pkg); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	var out []*nfv1.BuildStringCandidate
	for i := range pkg.Files {
		f := &pkg.Files[i]
		if f.Version != version {
			continue
		}
		if !isLinux64(f) {
			continue
		}
		buildStr := f.Build
		if buildStr == "" {
			buildStr = f.Attrs.Build
		}
		if buildStr == "" {
			continue
		}
		out = append(out, &nfv1.BuildStringCandidate{
			BuildString: buildStr,
			FullPin:     name + "=" + version + "=" + buildStr,
			Channel:     channel,
		})
	}
	return out, nil
}

// isLinux64 returns true for files targeting linux x86_64 or with an empty/null platform
// (older Anaconda.org entries omit platform).
func isLinux64(f *anacondaFile) bool {
	if f.Platform == "" && f.Arch == "" {
		return true
	}
	plat := strings.ToLower(f.Platform)
	arch := strings.ToLower(f.Arch)
	if plat == "linux-64" {
		return true
	}
	if plat == "linux" && (arch == "x86_64" || arch == "64" || arch == "") {
		return true
	}
	return false
}
