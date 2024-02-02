package ippool

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/harvester/webhook/pkg/server/admission"
	"github.com/rancher/wrangler/pkg/kv"
	"github.com/sirupsen/logrus"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"

	networkv1 "github.com/harvester/vm-dhcp-controller/pkg/apis/network.harvesterhci.io/v1alpha1"
	ctlcniv1 "github.com/harvester/vm-dhcp-controller/pkg/generated/controllers/k8s.cni.cncf.io/v1"
	ctlnetworkv1 "github.com/harvester/vm-dhcp-controller/pkg/generated/controllers/network.harvesterhci.io/v1alpha1"
	"github.com/harvester/vm-dhcp-controller/pkg/util"
	"github.com/harvester/vm-dhcp-controller/pkg/webhook"
)

type Validator struct {
	admission.DefaultValidator

	nadCache      ctlcniv1.NetworkAttachmentDefinitionCache
	vmnetcfgCache ctlnetworkv1.VirtualMachineNetworkConfigCache
}

func NewValidator(nadCache ctlcniv1.NetworkAttachmentDefinitionCache, vmnetcfgCache ctlnetworkv1.VirtualMachineNetworkConfigCache) *Validator {
	return &Validator{
		nadCache:      nadCache,
		vmnetcfgCache: vmnetcfgCache,
	}
}

func (v *Validator) Create(_ *admission.Request, newObj runtime.Object) error {
	ipPool := newObj.(*networkv1.IPPool)
	logrus.Infof("create ippool %s/%s", ipPool.Namespace, ipPool.Name)

	// sanity check
	poolInfo, err := util.LoadPool(ipPool)
	if err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkNAD(ipPool.Spec.NetworkName); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkPoolRange(poolInfo); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkServerIP(poolInfo); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkRouter(poolInfo); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	return nil
}

func (v *Validator) Update(_ *admission.Request, _, newObj runtime.Object) error {
	ipPool := newObj.(*networkv1.IPPool)

	if ipPool.DeletionTimestamp != nil {
		return nil
	}

	logrus.Infof("update ippool %s/%s", ipPool.Namespace, ipPool.Name)

	// sanity check
	poolInfo, err := util.LoadPool(ipPool)
	if err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	var allocatedIPAddrList []netip.Addr
	if ipPool.Status.IPv4 != nil {
		allocatedIPAddrList = util.LoadAllocated(ipPool.Status.IPv4.Allocated)
	}

	if err := v.checkNAD(ipPool.Spec.NetworkName); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkPoolRange(poolInfo); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkServerIP(poolInfo, allocatedIPAddrList...); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	if err := v.checkRouter(poolInfo); err != nil {
		return fmt.Errorf(webhook.CreateErr, "IPPool", ipPool.Namespace, ipPool.Name, err)
	}

	return nil
}

func (v *Validator) Delete(_ *admission.Request, oldObj runtime.Object) error {
	ipPool := oldObj.(*networkv1.IPPool)
	logrus.Infof("delete ippool %s/%s", ipPool.Namespace, ipPool.Name)

	if err := v.checkVmNetCfgs(ipPool); err != nil {
		return fmt.Errorf(webhook.DeleteErr, ipPool.Kind, ipPool.Namespace, ipPool.Name, err)
	}

	return nil
}

func (v *Validator) Resource() admission.Resource {
	return admission.Resource{
		Names:      []string{"ippools"},
		Scope:      admissionregv1.NamespacedScope,
		APIGroup:   networkv1.SchemeGroupVersion.Group,
		APIVersion: networkv1.SchemeGroupVersion.Version,
		ObjectType: &networkv1.IPPool{},
		OperationTypes: []admissionregv1.OperationType{
			admissionregv1.Create,
			admissionregv1.Update,
			admissionregv1.Delete,
		},
	}
}

func (v *Validator) checkNAD(namespacedName string) error {
	nadNamespace, nadName := kv.RSplit(namespacedName, "/")
	if nadNamespace == "" {
		nadNamespace = "default"
	}

	_, err := v.nadCache.Get(nadNamespace, nadName)
	return err
}

func (v *Validator) checkPoolRange(pi util.PoolInfo) error {
	if pi.StartIPAddr.IsValid() {
		if !pi.IPNet.Contains(pi.StartIPAddr.AsSlice()) {
			return fmt.Errorf("start ip %s is not within subnet", pi.StartIPAddr)
		}

		if pi.StartIPAddr.As4() == pi.NetworkIPAddr.As4() {
			return fmt.Errorf("start ip %s is the same as network ip", pi.StartIPAddr)
		}

		if pi.StartIPAddr.As4() == pi.BroadcastIPAddr.As4() {
			return fmt.Errorf("start ip %s is the same as broadcast ip", pi.StartIPAddr)
		}
	}

	if pi.EndIPAddr.IsValid() {
		if !pi.IPNet.Contains(pi.EndIPAddr.AsSlice()) {
			return fmt.Errorf("end ip %s is not within subnet", pi.EndIPAddr)
		}

		if pi.EndIPAddr.As4() == pi.NetworkIPAddr.As4() {
			return fmt.Errorf("end ip %s is the same as network ip", pi.EndIPAddr)
		}

		if pi.EndIPAddr.As4() == pi.BroadcastIPAddr.As4() {
			return fmt.Errorf("end ip %s is the same as broadcast ip", pi.EndIPAddr)
		}
	}
	return nil
}

func (v *Validator) checkServerIP(pi util.PoolInfo, allocatedIPs ...netip.Addr) error {
	if !pi.ServerIPAddr.IsValid() {
		return nil
	}

	if !pi.IPNet.Contains(pi.ServerIPAddr.AsSlice()) {
		return fmt.Errorf("server ip %s is not within subnet", pi.ServerIPAddr)
	}

	if pi.ServerIPAddr.As4() == pi.NetworkIPAddr.As4() {
		return fmt.Errorf("server ip %s cannot be the same as network ip", pi.ServerIPAddr)
	}

	if pi.ServerIPAddr.As4() == pi.BroadcastIPAddr.As4() {
		return fmt.Errorf("server ip %s cannot be the same as broadcast ip", pi.ServerIPAddr)
	}

	if pi.RouterIPAddr.IsValid() && pi.ServerIPAddr.As4() == pi.RouterIPAddr.As4() {
		return fmt.Errorf("server ip %s cannot be the same as router ip", pi.ServerIPAddr)
	}

	for _, ip := range allocatedIPs {
		if pi.ServerIPAddr == ip {
			return fmt.Errorf("server ip %s is already allocated", pi.ServerIPAddr)
		}
	}

	return nil
}

func (v *Validator) checkRouter(pi util.PoolInfo) error {
	if !pi.RouterIPAddr.IsValid() {
		return nil
	}

	if !pi.IPNet.Contains(pi.RouterIPAddr.AsSlice()) {
		return fmt.Errorf("router ip %s is not within subnet", pi.RouterIPAddr)
	}

	if pi.RouterIPAddr.As4() == pi.NetworkIPAddr.As4() {
		return fmt.Errorf("router ip %s is the same as network ip", pi.RouterIPAddr)
	}

	if pi.RouterIPAddr.As4() == pi.BroadcastIPAddr.As4() {
		return fmt.Errorf("router ip %s is the same as broadcast ip", pi.BroadcastIPAddr)
	}

	return nil
}

func (v *Validator) checkVmNetCfgs(ipPool *networkv1.IPPool) error {
	vmnetcfgGetter := util.VmnetcfgGetter{
		VmnetcfgCache: v.vmnetcfgCache,
	}
	vmNetCfgs, err := vmnetcfgGetter.WhoUseIPPool(ipPool)
	if err != nil {
		return err
	}

	logrus.Infof("%d vmnetcfg(s) associated", len(vmNetCfgs))

	if len(vmNetCfgs) > 0 {
		vmNetCfgNames := make([]string, 0, len(vmNetCfgs))
		for _, vmNetCfg := range vmNetCfgs {
			vmNetCfgNames = append(vmNetCfgNames, vmNetCfg.Namespace+"/"+vmNetCfg.Name)
		}
		return fmt.Errorf("it's still used by VirtualMachineNetworkConfig(s) %s, which must be removed at first", strings.Join(vmNetCfgNames, ", "))
	}
	return nil
}
