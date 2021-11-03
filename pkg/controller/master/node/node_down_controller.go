package node

import (
	"context"
	"fmt"
	"time"

	ctlcorev1 "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kv1 "kubevirt.io/client-go/api/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/config"
	v1 "github.com/harvester/harvester/pkg/generated/controllers/kubevirt.io/v1"
	"github.com/harvester/harvester/pkg/settings"
)

const (
	nodeDownControllerName = "node-down-controller"
	vmCreatorLabel         = "harvesterhci.io/creator"
)

// nodeDownHandler force deletes VMI's pod when a node is down, so VMI can be reschduled to anothor healthy node
type nodeDownHandler struct {
	nodes                       ctlcorev1.NodeController
	nodeCache                   ctlcorev1.NodeCache
	pods                        ctlcorev1.PodClient
	virtualMachineInstanceCache v1.VirtualMachineInstanceCache
}

// NodeDownRegister registers a controller to delete VMI when node is down
func NodeDownRegister(ctx context.Context, management *config.Management, options config.Options) error {
	nodes := management.CoreFactory.Core().V1().Node()
	pods := management.CoreFactory.Core().V1().Pod()
	setting := management.HarvesterFactory.Harvesterhci().V1beta1().Setting()
	vmis := management.VirtFactory.Kubevirt().V1().VirtualMachineInstance()
	nodeDownHandler := &nodeDownHandler{
		nodes:                       nodes,
		nodeCache:                   nodes.Cache(),
		pods:                        pods,
		virtualMachineInstanceCache: vmis.Cache(),
	}

	nodes.OnChange(ctx, nodeDownControllerName, nodeDownHandler.OnNodeChanged)
	setting.OnChange(ctx, nodeDownControllerName, nodeDownHandler.OnVMForceDeletionPolicyChanged)

	return nil
}

// OnNodeChanged monitors whether a node is ready or not
// Force delete a pod when all of the below conditions are meet:
// 1. VMDeletionPolicy is enabled.
// 2. A node has been down for more than VMDeletionPolicy.Period seconds
// 3. The owner of Pod is VirtualMachineInstance.
// 4. The Pod is on a down node.
func (h *nodeDownHandler) OnNodeChanged(key string, node *corev1.Node) (*corev1.Node, error) {
	if node == nil || node.DeletionTimestamp != nil {
		return node, nil
	}

	// get Ready condition
	cond := getNodeCondition(node.Status.Conditions, corev1.NodeReady)
	if cond == nil {
		return node, fmt.Errorf("can't find %s condition in node %s", corev1.NodeReady, node.Name)
	}

	// check whether node is healthy
	if cond.Status == corev1.ConditionTrue {
		return node, nil
	}

	// get VMForceDeletionPolicy setting
	vmForceDeletionPolicy, err := settings.DecodeVMForceDeletionPolicy(settings.VMForceDeletionPolicySet.Get())
	if err != nil {
		return node, err
	}

	if !vmForceDeletionPolicy.Enable {
		return node, nil
	}

	// if we haven't waited for vmForceDeletionPolicy.Period seconds, we enqueue event again
	if time.Since(cond.LastTransitionTime.Time) < time.Duration(vmForceDeletionPolicy.Period)*time.Second {
		deadline := cond.LastTransitionTime.Add(time.Duration(vmForceDeletionPolicy.Period) * time.Second)
		logrus.Debugf("Enqueue node event again at %v", deadline)
		h.nodes.EnqueueAfter(node.Name, time.Until(deadline))
		return node, nil
	}

	// get VMI pods on unhealthy node
	pods, err := h.pods.List(corev1.NamespaceAll, metav1.ListOptions{
		LabelSelector: labels.Set{
			kv1.AppLabel:   "virt-launcher",
			vmCreatorLabel: "harvester",
		}.String(),
		FieldSelector: "spec.nodeName=" + node.Name,
	})
	if err != nil {
		return node, err
	}

	gracePeriod := int64(0)
	for _, pod := range pods.Items {
		if err := h.pods.Delete(
			pod.Namespace,
			pod.Name,
			&metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
			}); err != nil {
			return node, err
		}

	}
	return node, nil
}

func (h *nodeDownHandler) OnVMForceDeletionPolicyChanged(key string, setting *harvesterv1.Setting) (*harvesterv1.Setting, error) {
	if setting == nil || setting.DeletionTimestamp != nil ||
		setting.Name != settings.VMForceDeletionPolicySettingName || setting.Value == "" {
		return setting, nil
	}

	// get VMForceDeletionPolicy setting
	vmForceDeletionPolicy, err := settings.DecodeVMForceDeletionPolicy(setting.Value)
	if err != nil {
		return setting, err
	}

	if !vmForceDeletionPolicy.Enable {
		return setting, nil
	}

	nodes, err := h.nodeCache.List(labels.Everything())
	if err != nil {
		return setting, err
	}

	for _, node := range nodes {
		cond := getNodeCondition(node.Status.Conditions, corev1.NodeReady)
		if cond != nil && cond.Status != corev1.ConditionTrue {
			h.nodes.Enqueue(node.Name)
		}
	}
	return setting, nil
}

func getNodeCondition(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) *corev1.NodeCondition {
	var cond *corev1.NodeCondition
	for _, c := range conditions {
		if c.Type == conditionType {
			cond = &c
			break
		}
	}
	return cond
}
