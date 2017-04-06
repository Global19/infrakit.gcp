package instance

import (
	"fmt"
	"net"
	"sort"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/infrakit.gcp/plugin/gcloud"
	instance_types "github.com/docker/infrakit.gcp/plugin/instance/types"
	"github.com/docker/infrakit.gcp/plugin/instance/util"
	"github.com/docker/infrakit/pkg/spi"
	"github.com/docker/infrakit/pkg/spi/instance"
	"github.com/docker/infrakit/pkg/types"
	"google.golang.org/api/compute/v1"
)

type plugin struct {
	API       gcloud.API
	namespace map[string]string
}

// NewGCEInstancePlugin creates a new GCE instance plugin for a given project
// and zone.
func NewGCEInstancePlugin(project, zone string, namespace map[string]string) instance.Plugin {
	api, err := gcloud.NewAPI(project, zone)
	if err != nil {
		log.Fatal(err)
	}

	return &plugin{
		API:       api,
		namespace: namespace,
	}
}

func (p *plugin) VendorInfo() *spi.VendorInfo {
	return &spi.VendorInfo{
		InterfaceSpec: spi.InterfaceSpec{
			Name:    "infrakit-instance-gcp",
			Version: "0.5.0",
		},
		URL: "https://github.com/docker/infrakit.gcp",
	}
}

func (p *plugin) Validate(req *types.Any) error {
	log.Debugln("validate", req.String())

	parsed := instance_types.Properties{}
	return req.Decode(&parsed)
}

func (p *plugin) Label(instance instance.ID, labels map[string]string) error {
	metadata := gcloud.TagsToMetaData(labels)

	return p.API.AddInstanceMetadata(string(instance), metadata)
}

func (p *plugin) Provision(spec instance.Spec) (*instance.ID, error) {
	properties, err := instance_types.ParseProperties(spec.Properties)
	if err != nil {
		return nil, err
	}

	settings := properties.InstanceSettings

	var name string
	if spec.LogicalID == nil {
		name = fmt.Sprintf("%s-%s", properties.NamePrefix, util.RandomSuffix(6))
	} else {
		// IP addresses / Logical ID
		// If the logical ID is set and is parsable as an IP address, then use that as the private IP
		// address. This will override the private IP address set in the struct because it's likely
		// that an orchestrator has determine the correct IP address to use.
		if ip := net.ParseIP(string(*spec.LogicalID)); len(ip) > 0 {
			settings.PrivateIP = ip.String()
			name = fmt.Sprintf("%s-%s", properties.NamePrefix, strings.Replace(ip.String(), ".", "-", -1))
		} else {
			name = string(*spec.LogicalID)
		}
	}

	id := instance.ID(name)

	// Parse the metadata in the spec, also merge in namespace tags to create the final metadata
	tags, err := instance_types.ParseTags(spec)
	if err != nil {
		return nil, err
	}
	_, tags = mergeTags(tags, p.namespace) // scope this resource with namespace tags

	// TODO - for now we overwrite, but support merging of MetaData field in the future, if the
	// user provided some.
	settings.MetaData = gcloud.TagsToMetaData(tags)

	if err = p.API.CreateInstance(name, settings); err != nil {
		return nil, err
	}

	for _, targetPool := range properties.TargetPools {
		if err = p.API.AddInstanceToTargetPool(targetPool, name); err != nil {
			return nil, err
		}
	}

	return &id, nil
}

func (p *plugin) Destroy(id instance.ID) error {
	err := p.API.DeleteInstance(string(id))

	log.Debugln("destroy", id, "err=", err)

	return err
}

func (p *plugin) DescribeInstances(tags map[string]string, properties bool) ([]instance.Description, error) {
	log.Debugln("describe-instances", tags)

	// apply the scoping namespace to restrict what we search for
	_, tags = mergeTags(tags, p.namespace)

	instances, err := p.API.ListInstances()
	if err != nil {
		return nil, err
	}

	log.Debugln("total count:", len(instances))

	result := []instance.Description{}

	for _, inst := range instances {
		instTags := gcloud.MetaDataToTags(inst.Metadata.Items)
		if gcloud.HasDifferentTag(tags, instTags) {
			continue
		}

		description := instance.Description{
			ID:        instance.ID(inst.Name),
			Tags:      instTags,
			LogicalID: logicalID(inst, instTags),
		}

		if properties {
			if any, err := types.AnyValue(inst); err == nil {
				description.Properties = any
			} else {
				log.Warningln("error encoding instance properties:", err)
			}
		}

		result = append(result, description)
	}

	log.Debugln("matching count:", len(result))

	return result, nil
}

func logicalID(inst *compute.Instance, tags map[string]string) *instance.LogicalID {
	_, present := tags[instance_types.InfrakitGCPVersion]
	if !present {
		// Instances created by old version of the plugin don't have a LogicalID metadata. We have to
		// infer whether it's a Pet or not using this heuristic:
		// When pets are deleted, we keep the disk. So a machine with a disk that's not auto-deleted is
		// assumed to be a pet and its logicalID is the name of the disk.
		if len(inst.Disks) > 0 && !inst.Disks[0].AutoDelete {
			id := instance.LogicalID(last(inst.Disks[0].Source))
			return &id
		}
		return nil
	}

	logicalID, present := tags[instance_types.InfrakitLogicalID]
	if present {
		id := instance.LogicalID(logicalID)
		return &id
	}

	return nil
}

func last(url string) string {
	parts := strings.Split(url, "/")
	return parts[len(parts)-1]
}

// mergeTags merges multiple maps of tags, implementing 'last write wins' for colliding keys.
// Returns a sorted slice of all keys, and the map of merged tags.  Sorted keys are particularly useful to assist in
// preparing predictable output such as for tests.
func mergeTags(tagMaps ...map[string]string) ([]string, map[string]string) {
	keys := []string{}
	tags := map[string]string{}
	for _, tagMap := range tagMaps {
		for k, v := range tagMap {
			if _, exists := tags[k]; exists {
				log.Warnf("Overwriting tag value for key %s", k)
			} else {
				keys = append(keys, k)
			}
			tags[k] = v
		}
	}
	sort.Strings(keys)
	return keys, tags
}
