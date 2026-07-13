package build

import (
	"errors"
	"fmt"
	"strings"

	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ValidateBuildRequest is NodeVault's final build gate. NodeKit may run the
// same checks earlier for UX, but NodeVault must re-check before Buildah sees
// client-authored Dockerfile content.
func ValidateBuildRequest(req *nfv1.BuildRequest) error {
	if req == nil {
		return errors.New("build request is required")
	}
	switch req.GetKind() {
	case nfv1.BuildKind_BUILD_KIND_TOOLFUNCTIONSPEC:
		return errors.New("BUILD_KIND_TOOLFUNCTIONSPEC is not supported by the Dockerfile build path")
	case nfv1.BuildKind_BUILD_KIND_UNSPECIFIED, nfv1.BuildKind_BUILD_KIND_TOOLSPEC:
		return validateToolSpecBuildRequest(req)
	default:
		return fmt.Errorf("unsupported build kind: %v", req.GetKind())
	}
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

func validateDockerfilePolicy(content string, allowRuntimeTools []string, allowRuntimeToolsReason string) error {
	lines := logicalDockerfileLines(content)
	totalFromCount := 0
	for _, line := range lines {
		if instruction, _ := dockerfileInstruction(line); instruction == "FROM" {
			totalFromCount++
		}
	}

	fromCount := 0
	for _, line := range lines {
		instruction, rest := dockerfileInstruction(line)
		switch instruction {
		case "FROM":
			fromCount++
			if err := validateFromInstruction(rest); err != nil {
				return err
			}
		case "USER":
			if err := validateUserInstruction(rest); err != nil {
				return err
			}
		case "RUN":
			if err := validateCondaPinsInRunInstruction(rest); err != nil {
				return err
			}
			// Runtime tool policy applies only to the final stage — earlier
			// build stages may freely use fetch/build tools.
			if fromCount == totalFromCount {
				if err := validateFinalStageRuntimeTools(rest, allowRuntimeTools, allowRuntimeToolsReason); err != nil {
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
func validateFinalStageRuntimeTools(rest string, allowRuntimeTools []string, allowRuntimeToolsReason string) error {
	allowed := make(map[string]bool, len(allowRuntimeTools))
	for _, tool := range allowRuntimeTools {
		allowed[tool] = true
	}
	for _, segment := range shellSegments(rest) {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		tool := cleanShellToken(fields[0])
		if !riskyRuntimeTools[tool] {
			continue
		}
		if allowed[tool] && strings.TrimSpace(allowRuntimeToolsReason) != "" {
			continue
		}
		return fmt.Errorf(
			"RUN %q uses runtime tool %q in the final image stage; "+
				"add it to allow_runtime_tools with allow_runtime_tools_reason, or remove it", rest, tool,
		)
	}
	return nil
}

// shellSegments splits a shell-form RUN instruction on &&, ||, |, and ;
// separators into individual command invocations.
func shellSegments(rest string) []string {
	var segments []string
	var current []string
	for _, field := range strings.Fields(rest) {
		if isShellSeparator(cleanShellToken(field)) {
			if len(current) > 0 {
				segments = append(segments, strings.Join(current, " "))
				current = nil
			}
			continue
		}
		current = append(current, field)
	}
	if len(current) > 0 {
		segments = append(segments, strings.Join(current, " "))
	}
	return segments
}

func validateCondaPinsInEnvironmentSpec(spec string) error {
	for _, raw := range strings.Split(spec, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimPrefix(line, "- ")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasSuffix(line, ":") {
			continue
		}
		token := firstToken(line)
		if token == "" || !strings.Contains(token, "=") {
			continue
		}
		if err := validateCondaPackagePin(token); err != nil {
			return fmt.Errorf("environment_spec: %w", err)
		}
	}
	return nil
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
			if token == "-c" || token == "--channel" {
				j++
				continue
			}
			if strings.HasPrefix(token, "-") || !strings.Contains(token, "=") {
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

func logicalDockerfileLines(content string) []string {
	var lines []string
	var current strings.Builder
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		continued := strings.HasSuffix(line, `\`)
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, `\`))
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(line)
		if !continued {
			lines = append(lines, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

func dockerfileInstruction(line string) (instruction, rest string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", ""
	}
	instruction = strings.ToUpper(fields[0])
	return instruction, strings.TrimSpace(line[len(fields[0]):])
}

func validateFromInstruction(rest string) error {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return errors.New("FROM instruction requires an image reference")
	}
	imageRef := ""
	for i := 0; i < len(fields); i++ {
		if strings.HasPrefix(fields[i], "--") {
			continue
		}
		imageRef = fields[i]
		break
	}
	if imageRef == "" {
		return errors.New("FROM instruction requires an image reference")
	}
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

func validateUserInstruction(rest string) error {
	user := strings.TrimSpace(rest)
	if user == "" {
		return errors.New("USER instruction requires a user")
	}
	if strings.Contains(user, "$") {
		return fmt.Errorf("USER %q must not use variables", user)
	}
	fields := strings.Fields(user)
	identity := fields[0]
	for _, part := range strings.Split(identity, ":") {
		if part == "0" || strings.EqualFold(part, "root") {
			return fmt.Errorf("USER %q must not be root", identity)
		}
	}
	return nil
}
