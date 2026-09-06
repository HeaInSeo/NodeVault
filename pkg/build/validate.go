package build

import (
	"errors"
	"fmt"
	"path"
	"strings"

	imagebuilder "github.com/openshift/imagebuilder"
	dockerfilecommand "github.com/openshift/imagebuilder/dockerfile/command"
	dockerfileparser "github.com/openshift/imagebuilder/dockerfile/parser"

	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ValidateBuildRequest is NodeVault's pre-build admission gate: a fast,
// static rejection of Dockerfile content that is obviously non-compliant
// before Buildah ever runs. NodeKit may run the same checks earlier for UX.
//
// This is deliberately NOT the final trust boundary. Static analysis of
// Dockerfile text cannot see what a base image already contains (e.g. a
// base image that ships curl pre-installed), and shell-form RUN content is
// inherently ambiguous in the general case. The authoritative check on what
// actually ends up in the built image belongs to post-build image content
// inspection (tracked separately; see docs/PLATFORM_SCHEDULE.md Sprint 10).
func ValidateBuildRequest(req *nfv1.BuildRequest) error {
	if req == nil {
		return errors.New("build request is required")
	}
	switch req.GetKind() {
	case nfv1.BuildKind_BUILD_KIND_TOOLFUNCTIONSPEC:
		return validateToolFunctionBuildRequest(req)
	case nfv1.BuildKind_BUILD_KIND_UNSPECIFIED, nfv1.BuildKind_BUILD_KIND_TOOLSPEC:
		return validateToolSpecBuildRequest(req)
	default:
		return fmt.Errorf("unsupported build kind: %v", req.GetKind())
	}
}

// validateToolFunctionBuildRequest is the W3 admission gate for a second-image
// function build (kind=2). The v1 raw_spec fields (kind=2, exact base image
// digest, non-empty script) were already validated by the frozen
// resolve.ParseRawSpecV1 parse; this validates the internal build request
// NodeVault assembled from them. The client-facing runtime-tools Dockerfile policy
// deliberately does NOT apply: the Dockerfile is NodeVault-generated (not
// client-supplied) and a function script may legitimately reference tools; the
// script is baked in via COPY, not executed as a final-stage RUN.
func validateToolFunctionBuildRequest(req *nfv1.BuildRequest) error {
	if !resolve.IsSHA256Digest(req.GetBaseImageDigest()) {
		return errors.New("base_image_digest must match sha256:<64 hex chars>")
	}
	if strings.TrimSpace(req.GetDockerfileContent()) == "" {
		return errors.New("function image build recipe is required")
	}
	return nil
}

func validateToolSpecBuildRequest(req *nfv1.BuildRequest) error {
	if req.GetBaseImageDigest() != "" && !resolve.IsSHA256Digest(req.GetBaseImageDigest()) {
		return errors.New("base_image_digest must match sha256:<64 hex chars>")
	}
	if strings.TrimSpace(req.GetDockerfileContent()) == "" {
		return errors.New("dockerfile_content is required")
	}
	dockerfileErr := validateDockerfilePolicy(
		req.GetDockerfileContent(), req.GetAllowRuntimeTools(), req.GetAllowRuntimeToolsReason(),
	)
	if dockerfileErr != nil {
		return dockerfileErr
	}
	if err := validateCondaPinsInEnvironmentSpec(req.GetEnvironmentSpec()); err != nil {
		return err
	}
	return nil
}

// parseDockerfileAST parses Dockerfile content with the same parser Buildah
// itself uses internally (go.podman.io/buildah imports
// github.com/openshift/imagebuilder for its own Dockerfile handling).
// Reusing it — instead of a hand-rolled line/instruction splitter — means
// NodeVault's understanding of comments, line continuations, escape
// directives, heredocs, and exec-form vs shell-form RUN instructions cannot
// drift from what Buildah will actually build.
func parseDockerfileAST(content string) (*dockerfileparser.Node, error) {
	result, err := dockerfileparser.Parse(strings.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("parse dockerfile: %w", err)
	}
	return result.AST, nil
}

// nodeArgs walks a parsed instruction's argument chain into a plain slice.
// For whitespace-delimited instructions (FROM, EXPOSE, ...) each element is
// one token. For exec-form RUN/CMD/ENTRYPOINT (RUN ["a", "b"]) each element
// is one JSON array entry — the literal argv Buildah will invoke, no shell
// involved. For single-string instructions (USER, WORKDIR, shell-form RUN)
// there is exactly one element holding the whole instruction body.
func nodeArgs(node *dockerfileparser.Node) []string {
	var args []string
	for n := node.Next; n != nil; n = n.Next {
		args = append(args, n.Value)
	}
	return args
}

func validateDockerfilePolicy(content string, allowRuntimeTools []string, allowRuntimeToolsReason string) error {
	ast, err := parseDockerfileAST(content)
	if err != nil {
		return err
	}

	totalFromCount := 0
	for _, node := range ast.Children {
		if node.Value == dockerfilecommand.From {
			totalFromCount++
		}
	}

	fromCount := 0
	for _, node := range ast.Children {
		switch node.Value {
		case dockerfilecommand.From:
			fromCount++
			if err := validateFromInstruction(nodeArgs(node)); err != nil {
				return err
			}
		case dockerfilecommand.User:
			if err := validateUserInstruction(nodeArgs(node)); err != nil {
				return err
			}
		case dockerfilecommand.Run:
			runText := strings.Join(nodeArgs(node), " ")
			if err := validateCondaPinsInRunInstruction(runText); err != nil {
				return err
			}
			// Runtime tool policy applies only to the final stage — earlier
			// build stages may freely use fetch/build tools.
			if fromCount == totalFromCount {
				if err := validateFinalStageRuntimeTools(node, allowRuntimeTools, allowRuntimeToolsReason); err != nil {
					return err
				}
			}
		}
	}
	if fromCount == 0 {
		return errors.New("dockerfile must contain at least one FROM instruction")
	}
	return nil
}

// riskyRuntimeTools are fetch/build tools that should not remain in a
// ToolSpec's final image stage by default, because ToolFunctionSpec images
// build on top of it. This is a static Dockerfile-text check: it catches
// RUN instructions that install or invoke these tools in the final stage,
// but cannot detect a base image that already ships one of them — that
// requires inspecting the built image's actual contents (Sprint 10).
//
// conda/mamba/micromamba are deliberately excluded even though the original
// requirement doc listed them: the Conda/Micromamba recipe variants render a
// single-stage Dockerfile whose only RUN line is exactly
// "RUN micromamba install <pkg>=<version>=<build>" — that IS the build
// mechanism, not a leftover fetch tool, and NodeKit has no multi-stage
// rendering or allow_runtime_tools exemption for these variants yet (per
// NodeKit's docs/NODEKIT_SOURCEBUILD_STRUCTURED_INTENT_DESIGN.md, only
// SourceBuild is getting multi-stage rendering, and it's not implemented
// yet either). Flagging them here would break currently-working production
// builds with no coordinated way to opt out. Revisit once NodeKit ships
// either multi-stage Conda/Micromamba rendering or passes
// allow_runtime_tools for them.
var riskyRuntimeTools = map[string]bool{
	"curl": true, "wget": true, "git": true, "ssh": true, "scp": true,
	"apt": true, "apt-get": true, "apk": true, "yum": true, "dnf": true,
	"gcc": true, "g++": true, "clang": true, "make": true, "cmake": true,
}

// validateFinalStageRuntimeTools rejects a final-stage RUN instruction that
// installs (e.g. "apt-get install curl") or directly invokes (e.g. "curl ...")
// a risky runtime tool, unless the tool is explicitly exempted via
// allow_runtime_tools with a non-empty allow_runtime_tools_reason.
func validateFinalStageRuntimeTools(
	node *dockerfileparser.Node, allowRuntimeTools []string, allowRuntimeToolsReason string,
) error {
	allowed := make(map[string]bool, len(allowRuntimeTools))
	for _, tool := range allowRuntimeTools {
		allowed[tool] = true
	}

	original := strings.Join(nodeArgs(node), " ")
	var segments [][]string
	if node.Attributes["json"] {
		// Exec-form RUN — args are the literal argv Buildah will invoke
		// directly (no shell), so there is exactly one "segment": the argv
		// itself. No shell tokenizing or metacharacter splitting applies.
		segments = [][]string{nodeArgs(node)}
	} else {
		var err error
		segments, err = shellCommandSegments(original)
		if err != nil {
			return fmt.Errorf("RUN %q: %w", original, err)
		}
	}

	for _, fields := range segments {
		if err := checkSegmentForRiskyTools(original, fields, allowed, allowRuntimeToolsReason); err != nil {
			return err
		}
	}
	return nil
}

// shellCommandSegments tokenizes a shell-form RUN instruction using the same
// word-splitting/quoting rules as Buildah's own Dockerfile parser (which
// imports github.com/openshift/imagebuilder directly), then splits the
// result on &&, ||, |, and ; into individual command invocations. Using the
// real tokenizer instead of strings.Fields means a separator that appears
// inside a quoted string, e.g. RUN echo "a && b", is not mistaken for a
// command boundary.
func shellCommandSegments(rest string) ([][]string, error) {
	words, err := imagebuilder.ProcessWords(rest, nil)
	if err != nil {
		return nil, err
	}
	var segments [][]string
	var current []string
	for _, word := range words {
		if isShellSeparator(word) {
			if len(current) > 0 {
				segments = append(segments, current)
				current = nil
			}
			continue
		}
		current = append(current, word)
	}
	if len(current) > 0 {
		segments = append(segments, current)
	}
	return segments, nil
}

// checkSegmentForRiskyTools inspects one command invocation (already split
// on shell separators). It unwraps command-prefix wrappers (env, exec) and
// recurses into "sh -c '<script>'"/"bash -c '<script>'" invocations so that
// a risky tool hidden behind a wrapper is still caught, matching what
// Buildah will actually execute.
func checkSegmentForRiskyTools(
	original string, fields []string, allowed map[string]bool, allowRuntimeToolsReason string,
) error {
	fields = unwrapCommandPrefix(fields)
	if len(fields) == 0 {
		return nil
	}

	if script, ok := shDashCArgument(fields); ok {
		innerSegments, err := shellCommandSegments(script)
		if err != nil {
			return fmt.Errorf("RUN %q: %w", original, err)
		}
		for _, inner := range innerSegments {
			if err := checkSegmentForRiskyTools(original, inner, allowed, allowRuntimeToolsReason); err != nil {
				return err
			}
		}
		return nil
	}

	tool := path.Base(fields[0])
	if !riskyRuntimeTools[tool] {
		return nil
	}
	if allowed[tool] && strings.TrimSpace(allowRuntimeToolsReason) != "" {
		return nil
	}
	return fmt.Errorf(
		"RUN %q uses runtime tool %q in the final image stage; "+
			"add it to allow_runtime_tools with allow_runtime_tools_reason, or remove it", original, tool,
	)
}

// unwrapCommandPrefix skips leading wrapper commands that re-invoke another
// program without being the risky tool themselves — "env" (sets environment
// variables before exec'ing its argument) and "exec" (replaces the current
// process with its argument) — so e.g. "RUN env FOO=bar curl ..." is
// evaluated as "curl ...", not "env ...".
func unwrapCommandPrefix(fields []string) []string {
	for len(fields) > 0 {
		switch path.Base(fields[0]) {
		case "exec":
			fields = fields[1:]
		case "env":
			fields = unwrapEnvFlags(fields[1:])
		default:
			return fields
		}
	}
	return fields
}

// unwrapEnvFlags skips past env's own flags and NAME=VALUE assignments to
// find the command env actually invokes, e.g.
// "env -i FOO=bar curl ..." -> ["curl", ...].
func unwrapEnvFlags(fields []string) []string {
	for len(fields) > 0 {
		f := fields[0]
		switch {
		case f == "-i" || f == "--ignore-environment":
			fields = fields[1:]
		case f == "-u" || f == "--unset":
			fields = fields[1:]
			if len(fields) > 0 {
				fields = fields[1:]
			}
		case strings.Contains(f, "=") && !strings.HasPrefix(f, "-"):
			fields = fields[1:]
		default:
			return fields
		}
	}
	return fields
}

// shDashCArgument recognizes "sh -c '<script>'" / "bash -c '<script>'" /
// "dash -c '<script>'" and returns the script argument, so callers can
// recurse into it instead of treating "sh"/"bash" as the invoked tool.
func shDashCArgument(fields []string) (string, bool) {
	switch path.Base(fields[0]) {
	case "sh", "bash", "dash":
	default:
		return "", false
	}
	for i := 1; i < len(fields); i++ {
		if fields[i] == "-c" {
			if i+1 < len(fields) {
				return fields[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// validateCondaPinsInEnvironmentSpec pin-validates only list items under a
// top-level "dependencies:" section of a conda environment.yml-shaped spec.
// This is a flat, section-name-tracking pass, not a real YAML parser: it
// does not handle multi-document specs or anchors/aliases, but conda
// environment.yml files don't use those either.
func validateCondaPinsInEnvironmentSpec(spec string) error {
	currentSection := ""
	for _, raw := range strings.Split(spec, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		trimmed := strings.TrimPrefix(line, "- ")
		if trimmed == line {
			// Not a list item — a top-level key, e.g. "name: bwa-env" or the
			// section header "dependencies:". Track which section follows.
			currentSection = strings.TrimSuffix(firstToken(line), ":")
			continue
		}
		if currentSection != "dependencies" {
			// e.g. a channel name under "channels:" — not a package spec.
			continue
		}
		if strings.HasSuffix(trimmed, ":") {
			// A nested section header under the list, e.g. "- pip:".
			continue
		}
		token := firstToken(trimmed)
		if token == "" {
			continue
		}
		if err := validateCondaPackagePin(token); err != nil {
			return fmt.Errorf("environment_spec: %w", err)
		}
	}
	return nil
}

// condaInstallValueFlags are conda/mamba/micromamba install flags that
// consume the following token as their value (not a package spec). Missing
// one here means its value gets misclassified as a bare package name and
// rejected — e.g. NodeKit's micromamba recipes always render
// "RUN micromamba install -n base -y <packages>" (micromamba requires an
// explicit target env; see NodeKit's RecipeRenderer.RenderCondaLike), so
// "-n" MUST be in this set or every micromamba build is rejected on "base".
var condaInstallValueFlags = map[string]bool{
	"-c": true, "--channel": true,
	"-n": true, "--name": true,
	"-p": true, "--prefix": true,
	"--clone": true,
}

func validateCondaPinsInRunInstruction(rest string) error {
	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		if !isCondaInstaller(fields[i]) {
			continue
		}
		if i+1 >= len(fields) || fields[i+1] != "install" {
			continue
		}
		for j := i + 2; j < len(fields); j++ {
			token := cleanShellToken(fields[j])
			if token == "" {
				continue
			}
			if isShellSeparator(token) {
				break
			}
			if condaInstallValueFlags[token] {
				j++
				continue
			}
			if strings.HasPrefix(token, "-") {
				continue
			}
			if err := validateCondaPackagePin(token); err != nil {
				return fmt.Errorf("RUN %s install: %w", fields[i], err)
			}
		}
	}
	return nil
}

func isCondaInstaller(token string) bool {
	switch cleanShellToken(token) {
	case "conda", "mamba", "micromamba":
		return true
	default:
		return false
	}
}

func validateCondaPackagePin(token string) error {
	pkg := cleanPackageToken(token)
	if pkg == "" || strings.Contains(pkg, "$") {
		return fmt.Errorf("package pin %q must not use variables", token)
	}
	if idx := strings.LastIndex(pkg, "::"); idx >= 0 {
		pkg = pkg[idx+2:]
	}
	if strings.Count(pkg, "=") < 2 {
		return fmt.Errorf("package pin %q must include name=version=build", token)
	}
	return nil
}

func firstToken(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func cleanShellToken(token string) string {
	return strings.Trim(token, `"'(),;`)
}

func cleanPackageToken(token string) string {
	return strings.Trim(cleanShellToken(token), "[]")
}

func isShellSeparator(token string) bool {
	switch token {
	case "&&", "||", "|", ";":
		return true
	default:
		return false
	}
}

func validateFromInstruction(args []string) error {
	if len(args) == 0 {
		return errors.New("FROM instruction requires an image reference")
	}
	imageRef := args[0]
	if strings.Contains(imageRef, "$") {
		return fmt.Errorf("FROM image %q must not use variables", imageRef)
	}
	if fromUsesLatestTag(imageRef) {
		return fmt.Errorf("FROM image %q must not use latest tag", imageRef)
	}
	digest := digestFromPinnedImage(imageRef)
	if digest == "" {
		return fmt.Errorf("FROM image %q must be pinned with @sha256 digest", imageRef)
	}
	if !resolve.IsSHA256Digest(digest) {
		return fmt.Errorf("FROM image %q digest must match sha256:<64 hex chars>", imageRef)
	}
	return nil
}

func digestFromPinnedImage(ref string) string {
	idx := strings.LastIndex(ref, "@")
	if idx == -1 || idx == len(ref)-1 {
		return ""
	}
	return strings.TrimSpace(ref[idx+1:])
}

func fromUsesLatestTag(ref string) bool {
	image := ref
	if idx := strings.LastIndex(image, "@"); idx >= 0 {
		image = image[:idx]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	return lastColon > lastSlash && image[lastColon+1:] == "latest"
}

func validateUserInstruction(args []string) error {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return errors.New("USER instruction requires a user")
	}
	user := strings.TrimSpace(args[0])
	if strings.Contains(user, "$") {
		return fmt.Errorf("USER %q must not use variables", user)
	}
	identity := strings.Fields(user)[0]
	for _, part := range strings.Split(identity, ":") {
		if part == "0" || strings.EqualFold(part, "root") {
			return fmt.Errorf("USER %q must not be root", identity)
		}
	}
	return nil
}
