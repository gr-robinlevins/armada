package scheduling

import (
	"github.com/G-Research/armada/internal/common"
	"github.com/G-Research/armada/pkg/api"
)

// nodeTypeAllocation stores the available resources for all nodes of a specific node type.
// The resource maps herein are the sum over all nodes of that type.
type nodeTypeAllocation struct {
	nodeType           api.NodeType
	availableResources common.ComputeResourcesFloat
	totalResources     common.ComputeResourcesFloat
	// AvailableResources map[string]resource.Quantity
	allocatedResourcesByPriority map[int32]common.ComputeResourcesFloat
}

type nodeTypeUsedResources map[*nodeTypeAllocation]common.ComputeResourcesFloat

func (r nodeTypeUsedResources) DeepCopy() map[*nodeTypeAllocation]common.ComputeResourcesFloat {
	result := map[*nodeTypeAllocation]common.ComputeResourcesFloat{}
	for k, v := range r {
		result[k] = v.DeepCopy()
	}
	return result
}

func (r nodeTypeUsedResources) Add(consumed nodeTypeUsedResources) {
	for nodeType, resources := range consumed {
		newResources := resources.DeepCopy()
		newResources.Add(r[nodeType])
		r[nodeType] = newResources
	}
}
