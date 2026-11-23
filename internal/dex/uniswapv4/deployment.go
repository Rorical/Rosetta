package uniswapv4

import "github.com/Rorical/Rosetta/internal/domain"

// DeploymentProvider binds loaders to a caller's chain. A provider returning
// zero addresses explicitly disables V4; no Arbitrum One fallback is applied.
type DeploymentProvider interface {
	V4Deployment() (manager, stateView domain.Address)
}

func Deployment(caller any) (manager, stateView domain.Address) {
	if provider, ok := caller.(DeploymentProvider); ok {
		return provider.V4Deployment()
	}
	return poolManagerAddress, stateViewAddress
}

func managerFor(caller any) domain.Address { manager, _ := Deployment(caller); return manager }
func viewFor(caller any) domain.Address    { _, view := Deployment(caller); return view }
