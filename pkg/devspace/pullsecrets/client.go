package pullsecrets

import (
	config2 "github.com/loft-sh/devspace/pkg/devspace/config"
	"github.com/loft-sh/devspace/pkg/devspace/config/generated"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/dependency/types"
	"github.com/loft-sh/devspace/pkg/devspace/docker"
	"github.com/loft-sh/devspace/pkg/devspace/hook"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/util/log"
)

// Client communicates with a registry
type Client interface {
	CreatePullSecrets() error
	CreatePullSecret(options *PullSecretOptions) error
}

// NewClient creates a client for a registry
func NewClient(config config2.Config, dependencies []types.Dependency, kubeClient kubectl.Client, dockerClient docker.Client, log log.Logger) Client {
	var (
		latest *latest.Config
		cache  *generated.CacheConfig
	)
	if config != nil {
		latest = config.Config()
		cache = config.Generated().GetActive()
	}

	return &client{
		config:       latest,
		cache:        cache,
		kubeClient:   kubeClient,
		dockerClient: dockerClient,
		hookExecuter: hook.NewExecuter(config, dependencies),
		log:          log,
	}
}

type client struct {
	config       *latest.Config
	cache        *generated.CacheConfig
	kubeClient   kubectl.Client
	dockerClient docker.Client
	hookExecuter hook.Executer
	log          log.Logger
}
