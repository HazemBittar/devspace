package dependency

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/devspace-cloud/devspace/pkg/devspace/build"
	"github.com/devspace-cloud/devspace/pkg/devspace/config/constants"
	"github.com/devspace-cloud/devspace/pkg/devspace/config/generated"
	"github.com/devspace-cloud/devspace/pkg/devspace/config/loader"
	"github.com/devspace-cloud/devspace/pkg/devspace/config/versions/latest"
	"github.com/devspace-cloud/devspace/pkg/devspace/dependency/util"
	"github.com/devspace-cloud/devspace/pkg/devspace/deploy"
	"github.com/devspace-cloud/devspace/pkg/devspace/docker"
	"github.com/devspace-cloud/devspace/pkg/devspace/kubectl"
	"github.com/devspace-cloud/devspace/pkg/devspace/pullsecrets"
	"github.com/devspace-cloud/devspace/pkg/util/git"
	"github.com/devspace-cloud/devspace/pkg/util/kubeconfig"
	"github.com/devspace-cloud/devspace/pkg/util/log"

	"github.com/pkg/errors"
)

// ResolverInterface defines the resolver interface that takes dependency configs and resolves them
type ResolverInterface interface {
	Resolve(update bool) ([]*Dependency, error)
}

// Resolver implements the resolver interface
type resolver struct {
	DependencyGraph *graph

	BasePath   string
	BaseConfig *latest.Config
	BaseCache  *generated.Config

	ConfigOptions *loader.ConfigOptions
	AllowCyclic   bool

	kubeLoader     kubeconfig.Loader
	client         kubectl.Client
	generatedSaver generated.ConfigLoader
	log            log.Logger
}

// NewResolver creates a new resolver for resolving dependencies
func NewResolver(baseConfig *latest.Config, baseCache *generated.Config, client kubectl.Client, allowCyclic bool, configOptions *loader.ConfigOptions, log log.Logger) (ResolverInterface, error) {
	var id string

	var kubeLoader kubeconfig.Loader
	if client == nil {
		kubeLoader = kubeconfig.NewLoader()
	} else {
		kubeLoader = client.KubeConfigLoader()
	}

	basePath, err := filepath.Abs(".")
	if err != nil {
		return nil, err
	}
	remote, err := git.GetRemote(basePath)
	if err == nil {
		id = remote
	} else {
		id = basePath
	}

	return &resolver{
		DependencyGraph: newGraph(newNode(id, nil)),

		BaseConfig: baseConfig,
		BaseCache:  baseCache,

		AllowCyclic:   allowCyclic,
		ConfigOptions: configOptions,

		// We only need that for saving
		kubeLoader:     kubeLoader,
		client:         client,
		generatedSaver: generated.NewConfigLoader(""),
		log:            log,
	}, nil
}

// Resolve implements interface
func (r *resolver) Resolve(update bool) ([]*Dependency, error) {
	currentWorkingDirectory, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err, "get current working directory")
	}

	err = r.resolveRecursive(currentWorkingDirectory, r.DependencyGraph.Root.ID, r.BaseConfig.Dependencies, update)
	if err != nil {
		if _, ok := err.(*cyclicError); ok {
			return nil, err
		}

		return nil, errors.Wrap(err, "resolve dependencies recursive")
	}

	// Save generated
	err = r.generatedSaver.Save(r.BaseCache)
	if err != nil {
		return nil, err
	}

	return r.buildDependencyQueue()
}

func (r *resolver) buildDependencyQueue() ([]*Dependency, error) {
	retDependencies := make([]*Dependency, 0, len(r.DependencyGraph.Nodes)-1)

	for len(r.DependencyGraph.Nodes) > 1 {
		next := r.DependencyGraph.getNextLeaf(r.DependencyGraph.Root)
		if next == r.DependencyGraph.Root {
			break
		}

		retDependencies = append(retDependencies, next.Data.(*Dependency))

		err := r.DependencyGraph.removeNode(next.ID)
		if err != nil {
			return nil, err
		}
	}

	return retDependencies, nil
}

func (r *resolver) resolveRecursive(basePath, parentID string, dependencies []*latest.DependencyConfig, update bool) error {
	for _, dependencyConfig := range dependencies {
		ID := util.GetDependencyID(basePath, dependencyConfig.Source, dependencyConfig.Profile)

		// Try to insert new edge
		if _, ok := r.DependencyGraph.Nodes[ID]; ok {
			err := r.DependencyGraph.addEdge(parentID, ID)
			if err != nil {
				if _, ok := err.(*cyclicError); ok {
					// Check if cyclic dependencies are allowed
					if !r.AllowCyclic {
						return err
					}
				} else {
					return err
				}
			}
		} else {
			dependency, err := r.resolveDependency(basePath, dependencyConfig, update)
			if err != nil {
				return err
			}

			_, err = r.DependencyGraph.insertNodeAt(parentID, ID, dependency)
			if err != nil {
				return errors.Wrap(err, "insert node")
			}

			// Load dependencies from dependency
			if dependencyConfig.IgnoreDependencies == nil || *dependencyConfig.IgnoreDependencies == false {
				if dependency.Config.Dependencies != nil && len(dependency.Config.Dependencies) > 0 {
					err = r.resolveRecursive(dependency.LocalPath, ID, dependency.Config.Dependencies, update)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func (r *resolver) resolveDependency(basePath string, dependency *latest.DependencyConfig, update bool) (*Dependency, error) {
	ID, localPath, err := util.DownloadDependency(basePath, dependency.Source, dependency.Profile, update, r.log)
	if err != nil {
		return nil, err
	}

	// Clone config options
	cloned, err := r.ConfigOptions.Clone()
	if err != nil {
		return nil, errors.Wrap(err, "clone config options")
	}

	cloned.Profile = dependency.Profile

	// Construct load path
	configPath := filepath.Join(localPath, constants.DefaultConfigPath)
	if dependency.Source.ConfigName != "" {
		configPath = filepath.Join(localPath, dependency.Source.ConfigName)
	}

	// Load config
	cloned.GeneratedConfig = r.BaseCache
	cloned.ConfigPath = configPath
	cloned.BasePath = loader.NewConfigLoader(r.ConfigOptions, r.log).ConfigPath()

	// Create the config loader
	var dConfig *latest.Config
	configLoader := loader.NewConfigLoader(cloned, r.log)
	if cloned.Profile == "" {
		dConfig, err = configLoader.LoadWithoutProfile()
	} else {
		dConfig, err = configLoader.Load()
	}
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("loading config for dependency %s", ID))
	}

	// parse the commands
	dCommands, err := configLoader.ParseCommands()
	if err != nil {
		return nil, errors.Wrap(err, "parse dependency commands")
	}

	// Override complete dev config
	dConfig.Dev = &latest.DevConfig{}

	// Check if we should skip building
	if dependency.SkipBuild != nil && *dependency.SkipBuild == true {
		dConfig.Images = map[string]*latest.ImageConfig{}
	}

	// Load dependency generated config
	gLoader := generated.NewConfigLoader(dependency.Profile)
	dGeneratedConfig, err := gLoader.LoadFromPath(filepath.Join(localPath, filepath.FromSlash(generated.ConfigPath)))
	if err != nil {
		return nil, errors.Errorf("Error loading generated config for dependency %s: %v", ID, err)
	}

	dGeneratedConfig.ActiveProfile = dependency.Profile
	generated.InitDevSpaceConfig(dGeneratedConfig, dependency.Profile)

	// Recreate client if necessary
	client := r.client
	if dependency.Namespace != "" {
		if r.client == nil {
			client, err = kubectl.NewClientFromContext("", dependency.Namespace, false, r.kubeLoader)
		} else {
			client, err = kubectl.NewClientFromContext(client.CurrentContext(), dependency.Namespace, false, r.kubeLoader)
		}
		if err != nil {
			return nil, errors.Wrap(err, "create new client")
		}
	}

	// Create docker client
	dockerClient, err := docker.NewClient(r.log)
	if err != nil {
		return nil, errors.Wrap(err, "create docker client")
	}

	// Create registry client for pull secrets
	registryClient := pullsecrets.NewClient(dConfig, client, dockerClient, r.log)

	return &Dependency{
		ID:        ID,
		LocalPath: localPath,

		Config:          dConfig,
		Commands:        dCommands,
		GeneratedConfig: dGeneratedConfig,

		DependencyConfig: dependency,
		DependencyCache:  r.BaseCache,

		kubeClient:     client,
		registryClient: registryClient,

		buildController:  build.NewController(dConfig, dGeneratedConfig.GetActive(), client),
		deployController: deploy.NewController(dConfig, dGeneratedConfig.GetActive(), client),
		generatedSaver:   gLoader,
	}, nil
}
