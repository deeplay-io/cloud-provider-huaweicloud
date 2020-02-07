/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package huaweicloud

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/cloud-provider"
	"k8s.io/klog"
)

// Cloud provider name: PaaS Web Services.
const (
	ProviderName               = "huaweicloud"
	ELBIDAnnotation            = "kubernetes.io/elb.id"
	ELBClassAnnotation         = "kubernetes.io/elb.class"
	ELBMarkAnnotation          = "kubernetes.io/elb.mark"
	ELBAutoCreateAnnotation    = "kubernetes.io/elb.autocreate"
	ELBEIPIDAnnotation         = "kubernetes.io/elb.eip-id"
	ELBLBAlgorithm             = "kubernetes.io/elb.lb-algorithm"
	ELBSessionAffinityMode     = "kubernetes.io/elb.session-affinity-mode"
	ELBSessionAffinityOption   = "kubernetes.io/elb.session-affinity-option"
	ELBEnterpriseAnnotationKey = "kubernetes.io/elb.enterpriseID"
	//persist auto create instance when loadbalancer instance has been deleted
	ELBPersistAutoCreate = "kubernetes.io/elb.persist-autocreate"

	ELBHealthCheckFlag   = "kubernetes.io/elb.health-check-flag"
	ELBHealthCheckOption = "kubernetes.io/elb.health-check-option"
	// HostNetworkAnnotationKey, when service annotation set to true, indicates that loadbalance(ELB)
	// will add backend server with container target port , so that users can access pod without NodePort,
	// which avoids a second hop for Nodeport, but risks host port conflict.
	// The default value is false.
	HostNetworkAnnotationKey = "kubernetes.io/hws-hostNetwork"

	ELBAnnotationPrefix = "kubernetes.io/elb."

	NodeSubnetIDLabelKey = "node.kubernetes.io/subnetid"

	MaxRetry            = 3
	HealthzCCE          = "cce-healthz"
	Attention           = "Attention! It is auto-generated by CCE service, do not modify!"
	ELBSessionNone      = ""
	ELBSessionSourceIP  = "SOURCE_IP"
	ELBPersistenTimeout = "persistence_timeout"

	ELBProtocolTCP  ELBProtocol = "TCP"
	ELBProtocolHTTP ELBProtocol = "HTTP"
	// protocol of udp type health monitor is UDP_CONNECT
	ELBHealthMonitorTypeUDP ELBProtocol = "UDP_CONNECT"

	ELBSessionSourceIPDefaultTimeout = 60
	ELBSessionSourceIPMinTimeout     = 1
	ELBSessionSourceIPMaxTimeout     = 60

	ELBSessionSource     ELBSessionPersistenceType = "SOURCE_IP"
	ELBSessionHTTPCookie ELBSessionPersistenceType = "HTTP_COOKIE"
	ELBSessionAppCookie  ELBSessionPersistenceType = "APP_COOKIE"

	MemberStatusNOMONITOR MemberStatus = "NO_MONITOR"
	MemberStatusONLINE    MemberStatus = "ONLINE"
	MemberStatusOFFLINE   MemberStatus = "OFFLINE"

	ELBAlgorithmNone             = ""
	ELBAlgorithmRoundRobin       = "ROUND_ROBIN"
	ELBAlgorithmLeastConnections = "LEAST_CONNECTIONS"
	ELBAlgorithmSourceIP         = "SOURCE_IP"

	ELBHealthMonitorOptionMinDelay   = 1
	ELBHealthMonitorOptionMaxDelay   = 50
	ELBHealthMonitorOptionMinTimeout = 1
	ELBHealthMonitorOptionMaxTimeout = 50
	ELBHealthMonitorOptionMinRetrys  = 1
	ELBHealthMonitorOptionMaxMRetrys = 10

	ELBHealthMonitorOptionDefaultDelay   = 5
	ELBHealthMonitorOptionDefaultTimeout = 10
	ELBHealthMonitorOptionDefaultRetrys  = 3

	ELBAlgorithmRR  ELBAlgorithm = "ROUND_ROBIN"
	ELBAlgorithmLC  ELBAlgorithm = "LEAST_CONNECTIONS"
	ELBAlgorithmSRC ELBAlgorithm = "SOURCE_IP"

	PodNetworkAttachmentDefinitionKey = "k8s.v1.cni.cncf.io/networks"
	DefaultEnterpriseProjectId        = "0"
)

type ELBProtocol string

type HTTPMethod string

type ELBSessionPersistenceType string

type ELBOperatingStatus string

type ELBProvisionStatus string

type MemberStatus string

type ELBAlgorithm string

type UUID struct {
	Id string `json:"id"`
}

type LBSessionAffinityOption struct {
	PersistenceTimeout int
}

type LBSessionAffinityConfig struct {
	LBSessionAffinityType   ELBSessionPersistenceType
	LBSessionAffinityOption LBSessionAffinityOption
}

type LBHealthCheckOption struct {
	CheckPort  *int
	Protocol   ELBProtocol
	UrlPath    string
	Delay      int
	Timeout    int
	MaxRetries int
}

type LBHealthMonitorConfig struct {
	LBHealthCheckStatus bool
	LBHealthCheckOption LBHealthCheckOption
}

type ServiceLBConfig struct {
	AgConfig *LBAlgorithmConfig
	SAConfig *LBSessionAffinityConfig
	HMConfig *LBHealthMonitorConfig
}

//example: kubernetes.io/elb.autocreate: '{"type":"public", "bandwidth_name":"bandwidth-d334","bandwidth_chargemode":"traffic","bandwidth_size":10,"bandwidth_sharetype":"PER","eip_type":"5_bgp"}'
type ElbAutoCreate struct {
	ELbName             string `json:"name,omitempty"`
	ElbType             string `json:"type,omitempty"`
	BandwidthName       string `json:"bandwidth_name,omitempty"`
	BandwidthChargemode string `json:"bandwidth_chargemode,omitempty"`
	BandwidthSize       int    `json:"bandwidth_size,omitempty"`
	BandwidthSharetype  string `json:"bandwidth_sharetype,omitempty"`
	EipType             string `json:"eip_type,omitempty"`
}

type LBAlgorithmConfig struct {
	LBAlgorithm ELBAlgorithm
}

type ELBSessionPersistence struct {
	Type               ELBSessionPersistenceType `json:"type,omitempty"`
	PersistenceTimeout int                       `json:"persistence_timeout,omitempty"`
	CookieName         string                    `json:"cookie_name,omitempty"`
}

type LBConfig struct {
	Apiserver        string       `json:"apiserver"`
	SecretName       string       `json:"secretName"`
	SignerType       string       `json:"signerType"`
	ELBAlgorithm     ELBAlgorithm `json:"elbAlgorithm"`
	TenantId         string       `json:"tenantId"`
	Region           string       `json:"region"`
	VPCId            string       `json:"vpcId"`
	SubnetId         string       `json:"subnetId"`
	ECSEndpoint      string       `json:"ecsEndpoint"`
	ELBEndpoint      string       `json:"elbEndpoint"`
	ALBEndpoint      string       `json:"albEndpoint"`
	GLBEndpoint      string       `json:"plbEndpoint"`
	NATEndpoint      string       `json:"natEndpoint"`
	VPCEndpoint      string       `json:"vpcEndpoint"`
	EnterpriseEnable string       `json:"enterpriseEnable"`
}

type PublicIp struct {
	Publicip  PublicIpSpec  `json:"publicip,omitempty"`
	Bandwidth BandwidthSpec `json:"bandwidth,omitempty"`
}

type PublicIpSpec struct {
	Type             string `json:"type,omitempty"`
	Status           string `json:"status,omitempty"`
	PublicIpAddress  string `json:"public_ip_address,omitempty"`
	PrivateIpAddress string `json:"private_ip_address,omitempty"`
	ID               string `json:"id,omitempty"`
	PortID           string `json:"port_id,omitempty"`
}

type PublicIps struct {
	Publicips []PublicIpSpec `json:"publicips,omitempty"`
}

type BandwidthSpec struct {
	Name       string `json:"name"`
	Size       int    `json:"size"`
	ShareType  string `json:"share_type"`
	ChargeMode string `json:"charge_mode"`
}

type SubnetItem struct {
	ID              string `json:"id,omitempty"`
	NeutronSubnetId string `json:"neutron_subnet_id,omitempty"`
	Cidr            string `json:"cidr,omitempty"`
}

type SubnetArr struct {
	Subnet SubnetItem `json:"subnet"`
}

type AvailableZoneList struct {
	AvailabilityZoneInfo []AZInfo `json:"availabilityZoneInfo,omitempty"`
}

type AZInfo struct {
	ZoneState ZoneState   `json:"zoneState,omitempty"`
	Hosts     interface{} `json:"hosts,omitempty"`
	ZoneName  string      `json:"zoneName,omitempty"`
}

type ZoneState struct {
	Available bool `json:"available,omitempty"`
}

type GenericNetworkAttachmentDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GenericNetworkAttachmentDefinitionSpec `json:"spec,omitempty"`
}

type GenericNetworkAttachmentDefinitionSpec struct {
	Config string `json:"config,omitempty"`
}

type GenericNetworkAttachmentDefinitionConfig struct {
	CniVersion string `json:"cniVersion,omitempty"`
	Name       string `json:"name,omitempty"`
	Type       string `json:"type,omitempty"`
	Bridge     string `json:"bridge,omitempty"`
	Args       Args   `json:"args,omitempty"`
}

type Args struct {
	Phynet         string `json:"phynet,omitempty"`
	VpcID          string `json:"vpc_id,omitempty"`
	SecurityGroups string `json:"securityGroups,omitempty"`
	SubnetID       string `json:"subnetID,omitempty"`
	Cidr           string `json:"cidr,omitempty"`
	AvailableZone  string `json:"availableZone,omitempty"`
	Region         string `json:"region,omitempty"`
}

var globalConfig *CloudConfig

type ServiceClient struct {
	Client   *http.Client
	Endpoint string
	Access   *AccessInfo
	TenantId string
}

type ELBListenerDescription struct {
	ClusterID string `json:"cluster_id,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
	Attention string `json:"attention,omitempty"`
}

/*
type Secret struct {
	Data struct {
		Credential string `json:"security.credential"`
	} `json:"data"`
}
*/

// Secret is a temporary solution for both support 'Permanent Security Credentials' and 'Temporary Security Credentials'.
// TODO(RainbowMango): Refactor later by a graceful way.
type Secret struct {
	Credential    string `json:"security.credential,omitempty"`
	AccessKey     string `json:"access,omitempty"`
	SecretKey     string `json:"secret,omitempty"`
	base64Decoded bool   `json:"-"`
}

// DecodeBase64 will decode all necessary fields with base64.
// TODO(RainbowMango): If decode partially success means some fields has been decoded and overwritten.
// Just limit this issue here and deal with it later with refactor actions.
func (s *Secret) DecodeBase64() error {
	if s.base64Decoded {
		panic(fmt.Sprintf("secret can not be decod twice"))
	}

	decodedBytes, err := base64.StdEncoding.DecodeString(s.Credential)
	if err != nil {
		klog.Errorf("Decode credential failed. error: %v", err)
		return fmt.Errorf("secret access key format is unexpected, %v", err)
	}
	s.Credential = string(decodedBytes)

	decodedBytes, err = base64.StdEncoding.DecodeString(s.AccessKey)
	if err != nil {
		klog.Errorf("Decode access key failed. error: %v", err)
		return fmt.Errorf("secret credential format is unexpected, %v", err)
	}
	s.AccessKey = string(decodedBytes)

	decodedBytes, err = base64.StdEncoding.DecodeString(s.SecretKey)
	if err != nil {
		klog.Errorf("Decode secret key failed. error: %v", err)
		return fmt.Errorf("secret secret key format is unexpected, %v", err)
	}
	s.SecretKey = string(decodedBytes)

	s.base64Decoded = true

	return nil
}

// PermanentSecurityCredentials represents 'Permanent Security Credentials'.
type PermanentSecurityCredentials struct {
	AccessKey string `json:"access"`
	SecretKey string `json:"secret"`
}

// SecurityCredential represents 'Temporary Security Credentials'.
type SecurityCredential struct {
	AccessKey     string    `json:"access"`
	SecretKey     string    `json:"secret"`
	SecurityToken string    `json:"securitytoken"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type HWSCloud struct {
	providers map[LoadBalanceVersion]cloudprovider.LoadBalancer
}

type ELBPoolDescription struct {
	ClusterID string `json:"cluster_id,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
	Port      int32  `json:"port,omitempty"`
	Attention string `json:"attention,omitempty"`
}

type CombinationPublicIPReq struct {
	NetworkType string        `json:"network_type"`
	Bandwidth   BandwidthSpec `json:"bandwidth"`
}

type Ipv6Bandwidth struct {
	ID string `json:"id"`
}

type LoadBalanceVersion int

const (
	VersionNotNeedLB LoadBalanceVersion = iota //if the service type is not LoadBalancer
	VersionELB
	VersionALB
	VersionPLB
	VersionNAT
)

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		hwsCloud, err := NewHWSCloud(config)
		if err != nil {
			return nil, err
		}
		return hwsCloud, nil
	})
}

func NewHWSCloud(config io.Reader) (*HWSCloud, error) {
	if config == nil {
		return nil, fmt.Errorf("huaweicloud provider config is nil")
	}

	globalConfig, err := ReadConf(config)
	if err != nil {
		klog.Errorf("Read configuration failed with error: %v", err)
		return nil, err
	}
	LogConf(globalConfig)

	clientConfig, err := clientcmd.BuildConfigFromFlags(globalConfig.LoadBalancer.Apiserver, "")
	if err != nil {
		return nil, err
	}

	kubeClient, err := corev1.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: corev1.New(kubeClient.RESTClient()).Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "hws-cloudprovider"})
	lrucache, err := lru.New(200)
	if err != nil {
		return nil, err
	}

	secretInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return kubeClient.Secrets(metav1.NamespaceAll).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return kubeClient.Secrets(metav1.NamespaceAll).Watch(options)
			},
		},
		&v1.Secret{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	secretInformer.AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			kubeSecret := obj.(*v1.Secret)
			if kubeSecret.Name == globalConfig.LoadBalancer.SecretName {
				key := kubeSecret.Namespace + "/" + kubeSecret.Name
				lrucache.Add(key, kubeSecret)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSecret := oldObj.(*v1.Secret)
			newSecret := newObj.(*v1.Secret)
			if newSecret.Name == globalConfig.LoadBalancer.SecretName {
				if reflect.DeepEqual(oldSecret.Data, newSecret.Data) {
					return
				}
				key := newSecret.Namespace + "/" + newSecret.Name
				lrucache.Add(key, newSecret)
			}
		},
		DeleteFunc: func(obj interface{}) {
			deleteSecret(obj, lrucache)
		},
	}, 30*time.Second)

	go secretInformer.Run(nil)

	if !cache.WaitForCacheSync(nil, secretInformer.HasSynced) {
		klog.Errorf("failed to wait for HWSCloud to be synced")
	}

	hws := &HWSCloud{
		providers: map[LoadBalanceVersion]cloudprovider.LoadBalancer{},
	}

	hws.providers[VersionELB] = &ELBCloud{lrucache: lrucache, config: &globalConfig.LoadBalancer, kubeClient: kubeClient, eventRecorder: recorder}
	hws.providers[VersionALB] = &ALBCloud{lrucache: lrucache, config: &globalConfig.LoadBalancer, kubeClient: kubeClient, eventRecorder: recorder, subnetMap: map[string]string{}}
	// TODO(RainbowMango): Support PLB later.
	// hws.providers[VersionPLB] = &PLBCloud{lrucache: lrucache, config: &globalConfig.LoadBalancer, kubeClient: kubeClient, clientPool: deprecateddynamic.NewDynamicClientPool(clientConfig), eventRecorder: recorder, subnetMap: map[string]string{}}
	hws.providers[VersionNAT] = &NATCloud{lrucache: lrucache, config: &globalConfig.LoadBalancer, kubeClient: kubeClient, eventRecorder: recorder}

	return hws, nil
}

func (h *HWSCloud) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	LBVersion, err := getLoadBalancerVersion(service)
	if err != nil {
		return nil, false, err
	}

	provider, exist := h.providers[LBVersion]
	if !exist {
		return nil, false, nil
	}

	return provider.GetLoadBalancer(ctx, clusterName, service)
}

func (h *HWSCloud) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return ""
}

func (h *HWSCloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	LBVersion, err := getLoadBalancerVersion(service)
	if err != nil {
		return nil, err
	}

	provider, exist := h.providers[LBVersion]
	if !exist {
		return nil, nil
	}

	return provider.EnsureLoadBalancer(ctx, clusterName, service, nodes)
}

func (h *HWSCloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	LBVersion, err := getLoadBalancerVersion(service)
	if err != nil {
		return err
	}

	provider, exist := h.providers[LBVersion]
	if !exist {
		return nil
	}

	return provider.UpdateLoadBalancer(ctx, clusterName, service, nodes)
}

func (h *HWSCloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	LBVersion, err := getLoadBalancerVersion(service)
	if err != nil {
		return err
	}

	provider, exist := h.providers[LBVersion]
	if !exist {
		return nil
	}

	return provider.EnsureLoadBalancerDeleted(ctx, clusterName, service)
}

func getLoadBalancerVersion(service *v1.Service) (LoadBalanceVersion, error) {
	class := service.Annotations[ELBClassAnnotation]

	switch class {
	case "elasticity":
		klog.Infof("Load balancer Version I for service %v", service.Name)
		return VersionELB, nil
	case "union", "":
		klog.Infof("Load balancer Version II for service %v", service.Name)
		return VersionALB, nil
	case "performance":
		klog.Infof("Load balancer Version III for service %v", service.Name)
		return VersionPLB, nil
	case "dnat":
		klog.Infof("DNAT for service %v", service.Name)
		return VersionNAT, nil
	default:
		return 0, fmt.Errorf("Load balancer version unknown")
	}
}

// type Instances interface {}

// ExternalID returns the cloud provider ID of the specified instance (deprecated).
func (h *HWSCloud) ExternalID(ctx context.Context, instance types.NodeName) (string, error) {
	return "", cloudprovider.NotImplemented
}

// List is an implementation of Instances.List.
func (h *HWSCloud) List(filter string) ([]types.NodeName, error) {
	return nil, nil
}

// type Routes interface {}

// ListRoutes is an implementation of Routes.ListRoutes
func (h *HWSCloud) ListRoutes(ctx context.Context, clusterName string) ([]*cloudprovider.Route, error) {
	return nil, nil
}

// CreateRoute is an implementation of Routes.CreateRoute
func (h *HWSCloud) CreateRoute(ctx context.Context, clusterName string, nameHint string, route *cloudprovider.Route) error {
	return nil
}

// DeleteRoute is an implementation of Routes.DeleteRoute
func (h *HWSCloud) DeleteRoute(ctx context.Context, clusterName string, route *cloudprovider.Route) error {
	return nil
}

// type Zones interface {}

// GetZone is an implementation of Zones.GetZone
func (h *HWSCloud) GetZone(ctx context.Context) (cloudprovider.Zone, error) {
	return cloudprovider.Zone{}, nil
}

// GetZoneByProviderID returns the Zone containing the current zone and locality region of the node specified by providerId
// This method is particularly used in the context of external cloud providers where node initialization must be down
// outside the kubelets.
func (h *HWSCloud) GetZoneByProviderID(ctx context.Context, providerID string) (cloudprovider.Zone, error) {
	return cloudprovider.Zone{}, nil
}

// GetZoneByNodeName returns the Zone containing the current zone and locality region of the node specified by node name
// This method is particularly used in the context of external cloud providers where node initialization must be down
// outside the kubelets.
func (h *HWSCloud) GetZoneByNodeName(ctx context.Context, nodeName types.NodeName) (cloudprovider.Zone, error) {
	return cloudprovider.Zone{}, nil
}

// type Interface interface {}

// Known-useless DNS search path.
var uselessDNSSearchRE = regexp.MustCompile(`^[0-9]+.google.internal.$`)

// ScrubDNS filters DNS settings for pods.
func (h *HWSCloud) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	// GCE has too many search paths by default. Filter the ones we know are useless.
	for _, s := range searches {
		if !uselessDNSSearchRE.MatchString(s) {
			srchOut = append(srchOut, s)
		}
	}
	return nameservers, srchOut
}

// HasClusterID returns true if the cluster has a clusterID
func (hws *HWSCloud) HasClusterID() bool {
	return true
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping activities within the cloud provider.
func (h *HWSCloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
}

// TCPLoadBalancer returns an implementation of TCPLoadBalancer for Huawei Web Services.
func (h *HWSCloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return h, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (h *HWSCloud) Instances() (cloudprovider.Instances, bool) {
	instance := &Instances{
		Auth: &globalConfig.Auth,
	}

	return instance, true
}

// Zones returns an implementation of Zones for Huawei Web Services.
func (h *HWSCloud) Zones() (cloudprovider.Zones, bool) {
	return h, true
}

// Clusters returns an implementation of Clusters for Huawei Web Services.
func (h *HWSCloud) Clusters() (cloudprovider.Clusters, bool) {
	return h, true
}

// Routes returns an implementation of Routes for Huawei Web Services.
func (h *HWSCloud) Routes() (cloudprovider.Routes, bool) {
	return h, true
}

// ProviderName returns the cloud provider ID.
func (h *HWSCloud) ProviderName() string {
	return ProviderName
}

// Session Affinity Type string
type HWSAffinityType string

const (
	// HWSAffinityTypeNone - no session affinity.
	HWSAffinityTypeNone HWSAffinityType = "None"
	// HWSAffinityTypeClientIP is the Client IP based.
	HWSAffinityTypeClientIP HWSAffinityType = "CLIENT_IP"
	// HWSAffinityTypeClientIPProto is the Client IP based.
	HWSAffinityTypeCookie HWSAffinityType = "COOKIE"
)

// type Clusters interface {}

// ListClusters is an implementation of Clusters.ListClusters
func (h *HWSCloud) ListClusters(ctx context.Context) ([]string, error) {
	return nil, nil
}

// Master is an implementation of Clusters.Master
func (h *HWSCloud) Master(ctx context.Context, clusterName string) (string, error) {
	return "", nil
}

//util functions

func GetListenerName(service *v1.Service) string {
	return string(service.UID)
}

//k8s-TCP/UDP-8080
func GetListenerNameV1(port *v1.ServicePort) string {
	return fmt.Sprintf("k8s_%s_%d", port.Protocol, port.Port)
}

func GetLoadbalancerName(service *v1.Service) string {
	return "cce-lb-" + string(service.UID)
}

// to suit for old version
// if the elb has been created with the old version
// its listener name is service.name+service.uid
func GetOldListenerName(service *v1.Service) string {
	return strings.Replace(service.Name+"_"+string(service.UID), ".", "_", -1)
}

func GetPoolNameV1(service *v1.Service, port *v1.ServicePort) string {
	serviceSubNs := service.Namespace
	if len(serviceSubNs) > 64 {
		serviceSubNs = service.Namespace[0:64]
	}
	serviceSubName := service.Name
	if len(serviceSubName) > 64 {
		serviceSubName = service.Name[0:64]
	}
	return fmt.Sprintf("%s_%s_%s-%d_%s-%d", "k8s", serviceSubNs, serviceSubName,
		port.Port, port.Protocol, port.Port)
}

// if the node not health, it will not be added to ELB
func CheckNodeHealth(node *v1.Node) (bool, error) {
	conditionMap := make(map[v1.NodeConditionType]*v1.NodeCondition)
	for i := range node.Status.Conditions {
		cond := node.Status.Conditions[i]
		conditionMap[cond.Type] = &cond
	}

	status := false
	if condition, ok := conditionMap[v1.NodeReady]; ok {
		if condition.Status == v1.ConditionTrue {
			status = true
		} else {
			status = false
		}
	}

	if node.Spec.Unschedulable {
		status = false
	}

	return status, nil
}

func GetHealthCheckPort(service *v1.Service) *v1.ServicePort {
	for _, port := range service.Spec.Ports {
		if port.Name == HealthzCCE {
			return &port
		}
	}
	return nil
}

func GetLBAlgorithm(service *v1.Service) string {
	return service.Annotations[ELBLBAlgorithm]
}

func GetSessionAffinityType(service *v1.Service) string {
	return service.Annotations[ELBSessionAffinityMode]
}

func GetSessionAffinityOptions(service *v1.Service) string {
	return service.Annotations[ELBSessionAffinityOption]
}

func GetHealthCheckFlag(server *v1.Service) string {
	return server.Annotations[ELBHealthCheckFlag]
}

func GetHealthCheckOption(service *v1.Service) string {
	return service.Annotations[ELBHealthCheckOption]
}

func GetPersistAutoCreate(service *v1.Service) bool {
	persist := service.Annotations[ELBPersistAutoCreate]
	if persist == "" || persist == "false" {
		return false
	} else {
		return true
	}
}

func deleteSecret(obj interface{}, lrucache *lru.Cache) {
	kubeSecret, ok := obj.(*v1.Secret)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Couldn't get object from tombstone %#v", obj)
			return
		}
		kubeSecret, ok = tombstone.Obj.(*v1.Secret)
		if !ok {
			klog.Errorf("Tombstone contained object that is not a secret %#v", obj)
			return
		}
	}

	if kubeSecret.Name == globalConfig.LoadBalancer.SecretName {
		key := kubeSecret.Namespace + "/" + kubeSecret.Name
		lrucache.Add(key, kubeSecret)
	}
}

func IsPodActive(p *v1.Pod) bool {
	if v1.PodSucceeded != p.Status.Phase &&
		v1.PodFailed != p.Status.Phase &&
		p.DeletionTimestamp == nil {
		for _, c := range p.Status.Conditions {
			if c.Type == v1.PodReady && c.Status == v1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func updateServiceStatus(
	kubeClient corev1.CoreV1Interface,
	eventRecorder record.EventRecorder,
	service *v1.Service) {
	for i := 0; i < MaxRetry; i++ {
		toUpdate := service.DeepCopy()
		mark, ok := toUpdate.Annotations[ELBMarkAnnotation]
		if !ok {
			mark = "1"
			if toUpdate.Annotations == nil {
				toUpdate.Annotations = map[string]string{}
			}
		} else {
			retry, err := strconv.Atoi(mark)
			if err != nil {
				mark = "1"
			} else {
				// always retry will send too many requests to apigateway, this maybe case ddos
				if retry >= MaxRetry {
					sendEvent(eventRecorder, "CreateLoadBalancerFailed", "Retry LoadBalancer configuration too many times", service)
					return
				}
				retry += 1
				mark = fmt.Sprintf("%d", retry)
			}
		}
		toUpdate.Annotations[ELBMarkAnnotation] = mark
		_, err := kubeClient.Services(service.Namespace).Update(toUpdate)
		if err == nil {
			return
		}
		// If the object no longer exists, we don't want to recreate it. Just bail
		// out so that we can process the delete, which we should soon be receiving
		// if we haven't already.
		if apierrors.IsNotFound(err) {
			klog.Infof("Not persisting update to service '%s/%s' that no longer exists: %v",
				service.Namespace, service.Name, err)
			return
		}

		if apierrors.IsConflict(err) {
			service, err = kubeClient.Services(service.Namespace).Get(service.Name, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Get service(%s/%s) error: %v", service.Namespace, service.Name, err)
				continue
			}
		}
	}
}

// if async job is success, need to init mark again
func updateServiceMarkIfNeeded(
	kubeClient corev1.CoreV1Interface,
	service *v1.Service,
	tryAgain bool) {
	for i := 0; i < MaxRetry; i++ {
		toUpdate := service.DeepCopy()
		_, ok := toUpdate.Annotations[ELBMarkAnnotation]
		if !ok {
			if !tryAgain {
				return
			}

			if toUpdate.Annotations == nil {
				toUpdate.Annotations = map[string]string{}
			}
			toUpdate.Annotations[ELBMarkAnnotation] = "0"
		} else {
			delete(toUpdate.Annotations, ELBMarkAnnotation)
		}

		_, err := kubeClient.Services(service.Namespace).Update(toUpdate)
		if err == nil {
			return
		}

		// If the object no longer exists, we don't want to recreate it. Just bail
		// out so that we can process the delete, which we should soon be receiving
		// if we haven't already.
		if apierrors.IsNotFound(err) {
			klog.Infof("Not persisting update to service '%s/%s' that no longer exists: %v",
				service.Namespace, service.Name, err)
			return
		}

		if apierrors.IsConflict(err) {
			service, err = kubeClient.Services(service.Namespace).Get(service.Name, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Get service(%s/%s) error: %v", service.Namespace, service.Name, err)
				continue
			}
		}
	}

}

func sendEvent(eventRecorder record.EventRecorder, title, msg string, service *v1.Service) {
	klog.Errorf("[%s/%s]%s", service.Namespace, service.Name, msg)
	eventRecorder.Event(service, v1.EventTypeWarning, title, fmt.Sprintf("Details: %s", msg))
}

func isNeedAutoCreateLB(service *v1.Service) bool {
	return service.Annotations[ELBIDAnnotation] == "" && service.Annotations[ELBAutoCreateAnnotation] != ""
}

func hasAutoCreateLB(service *v1.Service) bool {
	return service.Annotations[ELBIDAnnotation] != "" && service.Annotations[ELBAutoCreateAnnotation] != ""
}

func isHostNetworkService(service *v1.Service) bool {
	return service.Annotations[HostNetworkAnnotationKey] == "true"
}

func getLBAlgorithm(service *v1.Service) (ELBAlgorithm, error) {
	//service.Spec.Ports
	switch al := GetLBAlgorithm(service); al {
	case ELBAlgorithmRoundRobin, ELBAlgorithmNone: //default lb algorithm is round robin
		return ELBAlgorithmRR, nil
	case ELBAlgorithmLeastConnections:
		return ELBAlgorithmLC, nil
	case ELBAlgorithmSourceIP:
		return ELBAlgorithmSRC, nil
	default:
		return "", fmt.Errorf("LB Algorithm [%s] not support", al)
	}
}

func getHealthCheckFlag(service *v1.Service) (bool, error) {
	flag := GetHealthCheckFlag(service)
	switch flag {
	case "on", "":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid health check flag ,only support on/off")
	}
}
