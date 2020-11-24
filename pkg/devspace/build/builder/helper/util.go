package helper

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/devspace-cloud/devspace/pkg/devspace/build/builder/restart"

	logpkg "github.com/devspace-cloud/devspace/pkg/util/log"

	"github.com/devspace-cloud/devspace/pkg/devspace/config/versions/latest"
	"github.com/docker/docker/pkg/archive"
	"github.com/pkg/errors"
)

// DefaultDockerfilePath is the default dockerfile path to use
const DefaultDockerfilePath = "./Dockerfile"

// DockerfileTargetRegexTemplate is a template for a regex that finds build targets in a Dockerfile
const DockerfileTargetRegexTemplate = "(?i)(^|\n)\\s*FROM\\s+(\\S+)\\s+AS\\s+(%s)\\s*($|\n)"

// DefaultContextPath is the default context path to use
const DefaultContextPath = "./"

// GetDockerfileAndContext retrieves the dockerfile and context
func GetDockerfileAndContext(imageConf *latest.ImageConfig) (string, string) {
	var (
		dockerfilePath = DefaultDockerfilePath
		contextPath    = DefaultContextPath
	)

	if imageConf.Dockerfile != "" {
		dockerfilePath = imageConf.Dockerfile
	}

	if imageConf.Context != "" {
		contextPath = imageConf.Context
	}

	return dockerfilePath, contextPath
}

// InjectBuildScriptInContext will add the restart helper script to the build context
func InjectBuildScriptInContext(buildCtx io.ReadCloser) (io.ReadCloser, error) {
	now := time.Now()
	hdrTmpl := &tar.Header{
		Mode:       0777,
		Uid:        0,
		Gid:        0,
		ModTime:    now,
		AccessTime: now,
		ChangeTime: now,
	}
	fldTmpl := &tar.Header{
		Mode:       0777,
		Uid:        0,
		Gid:        0,
		ModTime:    now,
		AccessTime: now,
		ChangeTime: now,
		Typeflag:   tar.TypeDir,
	}

	buildCtx = archive.ReplaceFileTarWrapper(buildCtx, map[string]archive.TarModifierFunc{
		"/.devspace/.devspace": func(_ string, h *tar.Header, content io.Reader) (*tar.Header, []byte, error) {
			return fldTmpl, nil, nil
		},
		restart.ScriptContextPath: func(_ string, h *tar.Header, content io.Reader) (*tar.Header, []byte, error) {
			return hdrTmpl, []byte(restart.HelperScript), nil
		},
	})
	return buildCtx, nil
}

// OverwriteDockerfileInBuildContext will overwrite the dockerfile with the dockerfileCtx
func OverwriteDockerfileInBuildContext(dockerfileCtx io.ReadCloser, buildCtx io.ReadCloser, relDockerfile string) (io.ReadCloser, error) {
	file, err := ioutil.ReadAll(dockerfileCtx)
	dockerfileCtx.Close()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	hdrTmpl := &tar.Header{
		Mode:       0600,
		Uid:        0,
		Gid:        0,
		ModTime:    now,
		Typeflag:   tar.TypeReg,
		AccessTime: now,
		ChangeTime: now,
	}

	buildCtx = archive.ReplaceFileTarWrapper(buildCtx, map[string]archive.TarModifierFunc{
		// Overwrite docker file
		relDockerfile: func(_ string, h *tar.Header, content io.Reader) (*tar.Header, []byte, error) {
			return hdrTmpl, file, nil
		},
	})
	return buildCtx, nil
}

// RewriteDockerfile rewrites the given dockerfile contents with the new entrypoint cmd and target. It does also inject the restart
// helper if specified
func RewriteDockerfile(dockerfile string, entrypoint []string, cmd []string, additionalInstructions []string, target string, injectHelper bool, log logpkg.Logger) (string, error) {
	if len(entrypoint) == 0 && len(cmd) == 0 && !injectHelper && len(additionalInstructions) == 0 {
		return "", nil
	}
	if additionalInstructions == nil {
		additionalInstructions = []string{}
	}

	if injectHelper {
		data, err := ioutil.ReadFile(dockerfile)
		if err != nil {
			return "", err
		}

		oldEntrypoint, oldCmd, err := getLastestEntrypointAndCmd(string(data), target)
		if err != nil {
			return "", err
		}

		if len(entrypoint) == 0 {
			if len(oldEntrypoint) == 0 {
				if len(cmd) == 0 && len(oldCmd) == 0 {
					return "", errors.Errorf("cannot inject restart helper into Dockerfile because neither ENTRYPOINT nor CMD was found.\n\nHow to fix this:\n- Option A: Define an ENTRYPOINT (or CMD) in your Dockerfile\n- Option B: Set `images.*.entrypoint` option in your devspace.yaml\n- Option C: If you don't want to inject the restart helper, set `images.*.injectRestartHelper` to false")
				}
				log.Warn("Using CMD statement for injecting restart helper because ENTRYPOINT is missing in Dockerfile and `images.*.entrypoint` is also not configured")
			}

			entrypoint = oldEntrypoint
			if len(cmd) == 0 && len(oldCmd) > 0 {
				cmd = oldCmd
			}
		} else if len(cmd) == 0 && len(oldCmd) > 0 {
			cmd = oldCmd
		}

		entrypoint = append([]string{restart.ScriptPath}, entrypoint...)
		additionalInstructions = append(additionalInstructions, "COPY /.devspace /")
	}

	return CreateTempDockerfile(dockerfile, entrypoint, cmd, additionalInstructions, target)
}

// CreateTempDockerfile creates a new temporary dockerfile that appends a new entrypoint and cmd
func CreateTempDockerfile(dockerfile string, entrypoint []string, cmd []string, additionalLines []string, target string) (string, error) {
	if entrypoint == nil && cmd == nil && len(additionalLines) == 0 {
		return "", errors.New("entrypoint, cmd & additional lines are empty")
	}

	data, err := ioutil.ReadFile(dockerfile)
	if err != nil {
		return "", err
	}

	// Overwrite entrypoint and cmd
	tmpDir, err := ioutil.TempDir("", "example")
	if err != nil {
		return "", err
	}

	// add the new entrypoint
	newData, err := addNewEntrypoint(string(data), entrypoint, cmd, additionalLines, target)
	if err != nil {
		return "", errors.Wrap(err, "add entrypoint")
	}

	tmpfn := filepath.Join(tmpDir, "Dockerfile")
	if err := ioutil.WriteFile(tmpfn, []byte(newData), 0666); err != nil {
		return "", err
	}

	return tmpfn, nil
}

// GetDockerfileTargets returns an array of names of all targets defined in a given Dockerfile
func GetDockerfileTargets(dockerfile string) ([]string, error) {
	targets := []string{}

	if dockerfile == "" {
		dockerfile = DefaultDockerfilePath
	}

	data, err := ioutil.ReadFile(dockerfile)
	if err != nil {
		return targets, err
	}
	content := string(data)

	// Find all targets
	targetFinder, err := regexp.Compile(fmt.Sprintf(DockerfileTargetRegexTemplate, "\\S+"))
	if err != nil {
		return targets, err
	}

	rawTargets := targetFinder.FindAllStringSubmatch(content, -1)

	for _, target := range rawTargets {
		targets = append(targets, target[3])
	}

	return targets, nil
}

var nextFromFinder = regexp.MustCompile("(?i)\n\\s*FROM")

func addNewEntrypoint(content string, entrypoint []string, cmd []string, additionalLines []string, target string) (string, error) {
	entrypointStr := ""
	if len(additionalLines) > 0 {
		entrypointStr += "\n" + strings.Join(additionalLines, "\n")
	}
	if len(entrypoint) > 0 {
		entrypointStr += "\n\nENTRYPOINT [\"" + strings.Join(entrypoint, "\",\"") + "\"]\n"
	} else if entrypoint != nil {
		entrypointStr += "\n\nENTRYPOINT []\n"
	}
	if len(cmd) > 0 {
		entrypointStr += "\n\nCMD [\"" + strings.Join(cmd, "\",\"") + "\"]\n"
	} else if cmd != nil {
		entrypointStr += "\n\nCMD []\n"
	}

	if target == "" {
		return content + entrypointStr, nil
	}

	before, after, err := splitDockerfileAtTarget(content, target)
	if err != nil {
		return "", err
	}

	return before + entrypointStr + after, nil
}

func splitDockerfileAtTarget(content string, target string) (string, string, error) {
	// Find the target
	targetFinder, err := regexp.Compile(fmt.Sprintf(DockerfileTargetRegexTemplate, target))
	if err != nil {
		return "", "", err
	}

	matches := targetFinder.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		return "", "", errors.Errorf("Coulnd't find target '%s' in dockerfile", target)
	} else if len(matches) > 1 {
		return "", "", errors.Errorf("Multiple matches for target '%s' in dockerfile", target)
	}

	// Find the next FROM statement
	nextFrom := nextFromFinder.FindStringIndex(content[matches[0][1]:])
	if len(nextFrom) != 2 {
		return content, "", nil
	}

	return content[:matches[0][1]+nextFrom[0]], content[matches[0][1]+nextFrom[0]:], nil
}

var entrypointLinePattern = regexp.MustCompile(`(?i)^[\s]*ENTRYPOINT[\s]+(.+)$`)
var cmdLinePattern = regexp.MustCompile(`(?i)^[\s]*CMD[\s]+(.+)$`)

func getLastestEntrypointAndCmd(content string, target string) ([]string, []string, error) {
	if target == "" {
		return parseLastOccurence(content)
	}

	before, _, err := splitDockerfileAtTarget(content, target)
	if err != nil {
		return nil, nil, err
	}

	return parseLastOccurence(before)
}

func parseLastOccurence(content string) ([]string, []string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))

	var lastOccurenceEntrypoint []string
	var lastOccurenceCmd []string
	for scanner.Scan() {
		line := scanner.Text()

		// is ENTRYPOINT?
		if matches := entrypointLinePattern.FindStringSubmatch(line); len(matches) == 2 {
			// exec or shell form?
			if matches[1][0] == '[' {
				lastOccurenceEntrypoint = []string{}
				err := json.Unmarshal([]byte(matches[1]), &lastOccurenceEntrypoint)
				if err != nil {
					return nil, nil, errors.Errorf("error parsing %s: %v", matches[1], err)
				}
			} else {
				lastOccurenceEntrypoint = []string{"/bin/sh", "-c", matches[1]}
			}

			// reset CMD
			lastOccurenceCmd = nil
		} else if matches := cmdLinePattern.FindStringSubmatch(line); len(matches) == 2 {
			// exec or shell form?
			if matches[1][0] == '[' {
				lastOccurenceCmd = []string{}
				err := json.Unmarshal([]byte(matches[1]), &lastOccurenceCmd)
				if err != nil {
					return nil, nil, errors.Errorf("error parsing %s: %v", matches[1], err)
				}
			} else {
				lastOccurenceCmd = []string{"/bin/sh", "-c", matches[1]}
			}
		}
	}

	return lastOccurenceEntrypoint, lastOccurenceCmd, scanner.Err()
}
