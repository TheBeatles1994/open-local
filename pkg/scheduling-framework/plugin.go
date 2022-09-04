package plugin

import (
	"context"
	"fmt"
	"k8s.io/klog/v2"
	"math"
	"sync"

	localtype "github.com/alibaba/open-local/pkg"
	localclientset "github.com/alibaba/open-local/pkg/generated/clientset/versioned"
	informers "github.com/alibaba/open-local/pkg/generated/informers/externalversions"
	nodelocalstorageinformer "github.com/alibaba/open-local/pkg/generated/informers/externalversions/storage/v1alpha1"
	"github.com/alibaba/open-local/pkg/scheduling-framework/cache"
	"github.com/alibaba/open-local/pkg/utils"

	volumesnapshot "github.com/kubernetes-csi/external-snapshotter/client/v4/clientset/versioned"
	volumesnapshotinformersfactory "github.com/kubernetes-csi/external-snapshotter/client/v4/informers/externalversions"
	volumesnapshotinformers "github.com/kubernetes-csi/external-snapshotter/client/v4/informers/externalversions/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1informers "k8s.io/client-go/informers/core/v1"
	storagev1informers "k8s.io/client-go/informers/storage/v1"
	"k8s.io/client-go/kubernetes"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	clientgocache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

const (
	stateKey framework.StateKey = PluginName
)

type stateData struct {
	podVolumeInfo       *PodLocalVolumeInfo
	allocateStateByNode map[string] /*nodeName*/ *cache.NodeAllocateState
	reservedState       *cache.NodeAllocateState
	locker              sync.RWMutex
}

func (state *stateData) Clone() framework.StateData {
	return state
}

//by score
func (state *stateData) GetAllocateState(nodeName string) *cache.NodeAllocateState {
	state.locker.RLock()
	defer state.locker.RUnlock()
	if state.allocateStateByNode == nil {
		return nil
	}
	return state.allocateStateByNode[nodeName]
}

//add by filter
func (state *stateData) AddAllocateState(nodeName string, allocate *cache.NodeAllocateState) {
	state.locker.Lock()
	defer state.locker.Unlock()
	if state.allocateStateByNode == nil {
		state.allocateStateByNode = map[string]*cache.NodeAllocateState{}
	}
	state.allocateStateByNode[nodeName] = allocate
}

type LVMPVCInfo struct {
	vgName  string
	request int64
	pvc     *corev1.PersistentVolumeClaim
}

type DevicePVCInfo struct {
	mediaType localtype.MediaType
	request   int64
	pvc       *corev1.PersistentVolumeClaim
}

/*
	calculate by preFilter to prevent double calculating in filter
*/
type PodLocalVolumeInfo struct {
	lvmPVCsWithVgNameNotAllocated    []*LVMPVCInfo //local lvm PVC have vgName and  had not allocated before
	lvmPVCsSnapshot                  []*LVMPVCInfo
	lvmPVCsWithoutVgNameNotAllocated []*LVMPVCInfo    //local lvm PVC have no vgName and had not allocated before
	hddDevicePVCsNotAllocated        []*DevicePVCInfo //hdd type pvc had not allocated before
	ssdDevicePVCsNotAllocated        []*DevicePVCInfo //ssd type pvc had not allocated before
	inlineVolumes                    []*cache.InlineVolumeAllocated
}

func (info *PodLocalVolumeInfo) HaveLocalVolumes() bool {
	if info == nil {
		return false
	}
	return len(info.lvmPVCsWithVgNameNotAllocated)+len(info.lvmPVCsWithoutVgNameNotAllocated)+
		len(info.lvmPVCsSnapshot)+len(info.hddDevicePVCsNotAllocated)+len(info.ssdDevicePVCsNotAllocated)+len(info.inlineVolumes) > 0
}

type LocalPlugin struct {
	handle                 framework.Handle
	args                   OpenLocalArg
	allocateStrategy       AllocateStrategy
	nodeAntiAffinityWeight *localtype.NodeAntiAffinityWeight

	scLister           storagelisters.StorageClassLister
	coreV1Informers    corev1informers.Interface
	storageV1Informers storagev1informers.Interface
	localInformers     nodelocalstorageinformer.Interface
	snapshotInformers  volumesnapshotinformers.Interface

	kubeClientSet  kubernetes.Interface
	localClientSet localclientset.Interface
	snapClientSet  volumesnapshot.Interface

	cache *cache.NodeStorageAllocatedCache
}

const PluginName = "Open-Local"

type OpenLocalArg struct {
	KubeConfigPath       string `json:"kubeConfigPath,omitempty"`
	SchedulerStrategy    string `json:"schedulerStrategy,omitempty"`
	NodeAntiAffinityConf string `json:"nodeAntiAffinityConf,omitempty"`
}

var _ = framework.PreFilterPlugin(&LocalPlugin{})
var _ = framework.FilterPlugin(&LocalPlugin{})
var _ = framework.ScorePlugin(&LocalPlugin{})
var _ = framework.ReservePlugin(&LocalPlugin{})
var _ = framework.PreBindPlugin(&LocalPlugin{})

// NewLocalPlugin
func NewLocalPlugin(configuration runtime.Object, f framework.Handle) (framework.Plugin, error) {

	unknownObj, ok := configuration.(*runtime.Unknown)
	if !ok {
		return nil, fmt.Errorf("want args to be of type *runtime.Unknown, got %T", configuration)
	}
	args := OpenLocalArg{}
	if err := frameworkruntime.DecodeInto(unknownObj, &args); err != nil {
		return nil, err
	}
	nodeAntiAffinityWeight, err := utils.ParseWeight(args.NodeAntiAffinityConf)
	if err != nil {
		return nil, err
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", args.KubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("error building kubeconfig: %s", err.Error())
	}
	// client
	localClient, err := localclientset.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error building yoda clientset: %s", err.Error())
	}
	snapClient, err := volumesnapshot.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error building snapshot clientset: %s", err.Error())
	}

	// cache
	cxt := context.Background()
	localStorageInformerFactory := informers.NewSharedInformerFactory(localClient, 0)
	snapshotInformerFactory := volumesnapshotinformersfactory.NewSharedInformerFactory(snapClient, 0)

	nodeCache := cache.NewNodeStorageAllocatedCache(f.SharedInformerFactory().Core().V1())

	localPlugin := &LocalPlugin{
		handle:                 f,
		allocateStrategy:       GetAllocateStrategy(&args),
		nodeAntiAffinityWeight: nodeAntiAffinityWeight,

		cache:              nodeCache,
		coreV1Informers:    f.SharedInformerFactory().Core().V1(),
		scLister:           f.SharedInformerFactory().Storage().V1().StorageClasses().Lister(),
		storageV1Informers: f.SharedInformerFactory().Storage().V1(),
		localInformers:     localStorageInformerFactory.Csi().V1alpha1(),
		snapshotInformers:  snapshotInformerFactory.Snapshot().V1(),

		kubeClientSet:  f.ClientSet(),
		localClientSet: localClient,
		snapClientSet:  snapClient,
	}

	localStorageInformer := localStorageInformerFactory.Csi().V1alpha1().NodeLocalStorages().Informer()
	localStorageInformer.AddEventHandler(clientgocache.ResourceEventHandlerFuncs{
		AddFunc:    localPlugin.OnNodeLocalStorageAdd,
		UpdateFunc: localPlugin.OnNodeLocalStorageUpdate,
	})

	pvInformer := f.SharedInformerFactory().Core().V1().PersistentVolumes().Informer()
	pvInformer.AddEventHandler(clientgocache.ResourceEventHandlerFuncs{
		AddFunc:    localPlugin.OnPVAdd,
		UpdateFunc: localPlugin.OnPVUpdate,
		DeleteFunc: localPlugin.OnPVDelete,
	})
	pvcInformer := f.SharedInformerFactory().Core().V1().PersistentVolumeClaims().Informer()
	pvcInformer.AddEventHandler(clientgocache.ResourceEventHandlerFuncs{
		AddFunc:    localPlugin.OnPVCAdd,
		UpdateFunc: localPlugin.OnPVCUpdate,
		DeleteFunc: localPlugin.OnPVCDelete,
	})
	podInformer := f.SharedInformerFactory().Core().V1().Pods().Informer()
	podInformer.AddEventHandler(clientgocache.ResourceEventHandlerFuncs{
		AddFunc:    localPlugin.OnPodAdd,
		UpdateFunc: localPlugin.OnPodUpdate,
		DeleteFunc: localPlugin.OnPodDelete,
	})

	localStorageInformerFactory.Start(cxt.Done())
	localStorageInformerFactory.WaitForCacheSync(cxt.Done())
	snapshotInformerFactory.Start(cxt.Done())
	snapshotInformerFactory.WaitForCacheSync(cxt.Done())

	return localPlugin, nil
}

// Name returns name of the plugin. It is used in logs, etc.
func (plugin *LocalPlugin) Name() string {
	return PluginName
}

type CacheUnit struct {
	name     string
	kind     string
	poolName string
	request  uint64
}

func (plugin *LocalPlugin) PreFilter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod) *framework.Status {
	podVolumeInfo, err := plugin.getPodLocalVolumeInfos(pod)
	if err != nil {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}
	state.Write(stateKey, &stateData{podVolumeInfo: podVolumeInfo, allocateStateByNode: map[string]*cache.NodeAllocateState{}})
	return framework.NewStatus(framework.Success)
}

func (plugin *LocalPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

//TODO This plugin can't get staticBindings pvc bound by volume_binding plugin here, so node storage that have no space but exist matchingVolume may also fail
func (plugin *LocalPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	nodeName := nodeInfo.Node().Name

	podVolumeInfo, err := plugin.getPodVolumeInfoFromState(state)
	if err != nil {
		return framework.AsStatus(err)
	}
	//not use local pv, return success
	if !podVolumeInfo.HaveLocalVolumes() {
		return framework.NewStatus(framework.Success)
	}

	err = plugin.filterBySnapshot(nodeName, podVolumeInfo.lvmPVCsSnapshot)
	if err != nil {
		return framework.AsStatus(err)
	}

	nodeAllocate, err := plugin.preAllocate(pod, podVolumeInfo, nodeName)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	stateData, err := plugin.getState(state)
	if err != nil {
		return framework.AsStatus(err)
	}
	stateData.AddAllocateState(nodeName, nodeAllocate)
	return framework.NewStatus(framework.Success)
}

func (plugin *LocalPlugin) Score(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string) (int64, *framework.Status) {
	allocateInfo, err := plugin.getNodeAllocateUnitFromState(state, nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	if allocateInfo == nil || !allocateInfo.Units.HaveLocalUnits() {
		if plugin.cache.IsLocalNode(nodeName) {
			return int64(utils.MinScore), framework.NewStatus(framework.Success)
		}
		return int64(utils.MaxScore), framework.NewStatus(framework.Success)
	}

	if allocateInfo.NodeStorageAllocatedByUnits == nil {
		return int64(utils.MinScore), framework.NewStatus(framework.Success)
	}

	scoreByCapacity := plugin.scoreByCapacity(allocateInfo)

	scoreByCount := plugin.scoreByCount(allocateInfo)

	scoreByAntiAffinity := plugin.scoreByNodeAntiAffinity(allocateInfo)
	return scoreByCapacity + scoreByCount + scoreByAntiAffinity, framework.NewStatus(framework.Success)
}

//PVC which will be bound as a staticBindings at step volume_binding.PreBind, finally allocate by pv and no need revert by pvc
func (plugin *LocalPlugin) Reserve(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string) *framework.Status {

	stateData, err := plugin.getState(state)
	if err != nil {
		return framework.AsStatus(err)
	}
	podVolumeInfo := stateData.podVolumeInfo
	//not use local pv, return success
	if podVolumeInfo == nil || !podVolumeInfo.HaveLocalVolumes() {
		return framework.NewStatus(framework.Success)
	}

	preAllocate, err := plugin.preAllocate(pod, podVolumeInfo, nodeName)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}

	//reset allocated size, and will re-allocate by assume;
	preAllocate.Units.ResetAllocatedSize()
	err = plugin.cache.Reserve(preAllocate)
	stateData.reservedState = preAllocate
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	return framework.NewStatus(framework.Success)
}

func (plugin *LocalPlugin) Unreserve(ctx context.Context, state *framework.CycleState, p *corev1.Pod, nodeName string) {
	reservedAllocate, err := plugin.getReserveState(state, nodeName)
	if err != nil {
		klog.Errorf("get AllocateUnitFromState for node %s error : %v", nodeName, err)
		return
	}
	if reservedAllocate == nil {
		return
	}
	// if allocated size
	plugin.cache.Unreserve(reservedAllocate)
	return
}

//TODO 1) staticBindings PVC which bound by volume_binding plugin may patch a wrong VG or Device to NLS.
//TODO 1) such as pvc allocated with VG1 OR Device1 by scheduler, but bound by volume_binding with  pv of VG2 OR Device2
//TODO 2) if Prebind step error, we will not revert patch info of nls, it can update next schedule cycle
func (plugin *LocalPlugin) PreBind(ctx context.Context, state *framework.CycleState, p *corev1.Pod, nodeName string) *framework.Status {
	reservedAllocate, err := plugin.getReserveState(state, nodeName)
	if err != nil {
		return framework.AsStatus(err)
	}
	if reservedAllocate == nil || reservedAllocate.Units == nil {
		return framework.NewStatus(framework.Success)
	}

	lvmAllocates := reservedAllocate.Units.LVMPVCAllocateUnits
	deviceAllocates := reservedAllocate.Units.DevicePVCAllocateUnits

	var pvcInfos []localtype.NodeStoragePVCAllocateInfo
	for _, allocate := range lvmAllocates {
		if allocate.VGName != "" && allocate.Allocated > 0 {
			pvcInfos = append(pvcInfos, localtype.NodeStoragePVCAllocateInfo{
				PVCNameSpace: allocate.PVCNamespace,
				PVCName:      allocate.PVCName,
				PVAllocatedInfo: localtype.PVAllocatedInfo{
					VGName:     allocate.VGName,
					VolumeType: string(localtype.VolumeTypeLVM),
				},
			})
		}
	}
	for _, allocate := range deviceAllocates {
		if allocate.DeviceName != "" && allocate.Allocated > 0 {
			pvcInfos = append(pvcInfos, localtype.NodeStoragePVCAllocateInfo{
				PVCNameSpace: allocate.PVCNamespace,
				PVCName:      allocate.PVCName,
				PVAllocatedInfo: localtype.PVAllocatedInfo{
					DeviceName: allocate.DeviceName,
					VolumeType: string(localtype.VolumeTypeDevice),
				},
			})
		}
	}
	err = plugin.addAllocatedInfoToNLS(nodeName, pvcInfos)
	if err != nil {
		klog.Errorf("patch allocate info(%#v) to nls(%s) fail for pod(%s)", pvcInfos, nodeName, p.UID, err.Error())
		return framework.AsStatus(err)
	}
	return framework.NewStatus(framework.Success)
}

// ScoreExtensions of the Score plugin.
func (plugin *LocalPlugin) ScoreExtensions() framework.ScoreExtensions {
	return plugin
}

// NormalizeScore invoked after scoring all nodes.
func (plugin *LocalPlugin) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Find highest and lowest scores.
	var highest int64 = -math.MaxInt64
	var lowest int64 = math.MaxInt64
	for _, nodeScore := range scores {
		if nodeScore.Score > highest {
			highest = nodeScore.Score
		}
		if nodeScore.Score < lowest {
			lowest = nodeScore.Score
		}
	}

	// Transform the highest to lowest score range to fit the framework's min to max node score range.
	oldRange := highest - lowest
	newRange := framework.MaxNodeScore - framework.MinNodeScore
	for i, nodeScore := range scores {
		if oldRange == 0 {
			scores[i].Score = framework.MinNodeScore
		} else {
			scores[i].Score = ((nodeScore.Score - lowest) * newRange / oldRange) + framework.MinNodeScore
		}
	}

	return nil
}

func (plugin *LocalPlugin) ReserveForInlineVolumeOnly(nodeName string, pod *corev1.Pod) error {
	inlineVolumeAllocateUnits, err := plugin.getInlineVolumeAllocates(pod)
	if err != nil {
		return err
	}
	if len(inlineVolumeAllocateUnits) > 0 {
		return plugin.cache.ReserveInlineVolumeOnly(nodeName, string(pod.UID), inlineVolumeAllocateUnits)
	}
	return nil
}

func (plugin *LocalPlugin) UnreserveInlineVolumeOnly(nodeName string, pod *corev1.Pod) {
	plugin.cache.UnreserveInlineVolumeOnly(nodeName, string(pod.UID))
}

func (plugin *LocalPlugin) getPodVolumeInfoFromState(cs *framework.CycleState) (*PodLocalVolumeInfo, error) {
	state, err := plugin.getState(cs)
	if err != nil {
		return nil, err
	}
	return state.podVolumeInfo, nil
}

func (plugin *LocalPlugin) getReserveState(cs *framework.CycleState, nodeName string) (*cache.NodeAllocateState, error) {
	state, err := plugin.getState(cs)
	if err != nil {
		return nil, err
	}
	return state.reservedState, nil
}

func (plugin *LocalPlugin) getNodeAllocateUnitFromState(cs *framework.CycleState, nodeName string) (*cache.NodeAllocateState, error) {
	state, err := plugin.getState(cs)
	if err != nil {
		return nil, err
	}
	return state.GetAllocateState(nodeName), nil
}

func (plugin *LocalPlugin) getState(cs *framework.CycleState) (*stateData, error) {
	state, err := cs.Read(stateKey)
	if err != nil {
		return nil, err
	}
	stateData, ok := state.(*stateData)
	if !ok {
		return nil, fmt.Errorf("unable to convert state into stateData")
	}
	return stateData, nil
}
