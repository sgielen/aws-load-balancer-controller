package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"regexp"
	elbv2api "sigs.k8s.io/aws-load-balancer-controller/apis/elbv2/v1beta1"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/annotations"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	elbv2model "sigs.k8s.io/aws-load-balancer-controller/pkg/model/elbv2"
	"sort"
	"strconv"
	"strings"
)

const (
	tgAttrsProxyProtocolV2Enabled  = "proxy_protocol_v2.enabled"
	tgAttrsPreserveClientIPEnabled = "preserve_client_ip.enabled"
	healthCheckPortTrafficPort     = "traffic-port"
)

func (t *defaultModelBuildTask) buildTargetGroup(ctx context.Context, port corev1.ServicePort, tgProtocol elbv2model.Protocol, scheme elbv2model.LoadBalancerScheme) (*elbv2model.TargetGroup, error) {
	svcPort := intstr.FromInt(int(port.Port))
	tgResourceID := t.buildTargetGroupResourceID(k8s.NamespacedName(t.service), svcPort)
	if targetGroup, exists := t.tgByResID[tgResourceID]; exists {
		return targetGroup, nil
	}
	targetType, err := t.buildTargetType(ctx)
	if err != nil {
		return nil, err
	}
	healthCheckConfig, err := t.buildTargetGroupHealthCheckConfig(ctx, targetType)
	if err != nil {
		return nil, err
	}
	tgAttrs, err := t.buildTargetGroupAttributes(ctx)
	if err != nil {
		return nil, err
	}
	preserveClientIP, err := t.buildPreserveClientIPFlag(ctx, targetType, tgAttrs)
	if err != nil {
		return nil, err
	}
	tgSpec, err := t.buildTargetGroupSpec(ctx, tgProtocol, targetType, port, healthCheckConfig, tgAttrs)
	if err != nil {
		return nil, err
	}
	targetGroup := elbv2model.NewTargetGroup(t.stack, tgResourceID, tgSpec)
	_, err = t.buildTargetGroupBinding(ctx, targetGroup, preserveClientIP, port, healthCheckConfig, scheme)
	if err != nil {
		return nil, err
	}
	t.tgByResID[tgResourceID] = targetGroup
	return targetGroup, nil
}

func (t *defaultModelBuildTask) buildTargetGroupSpec(ctx context.Context, tgProtocol elbv2model.Protocol, targetType elbv2model.TargetType,
	port corev1.ServicePort, healthCheckConfig *elbv2model.TargetGroupHealthCheckConfig, tgAttrs []elbv2model.TargetGroupAttribute) (elbv2model.TargetGroupSpec, error) {
	tags, err := t.buildTargetGroupTags(ctx)
	if err != nil {
		return elbv2model.TargetGroupSpec{}, err
	}
	targetPort := t.buildTargetGroupPort(ctx, targetType, port)
	tgName := t.buildTargetGroupName(ctx, intstr.FromInt(int(port.Port)), targetPort, targetType, tgProtocol, healthCheckConfig)
	return elbv2model.TargetGroupSpec{
		Name:                  tgName,
		TargetType:            targetType,
		Port:                  targetPort,
		Protocol:              tgProtocol,
		HealthCheckConfig:     healthCheckConfig,
		TargetGroupAttributes: tgAttrs,
		Tags:                  tags,
	}, nil
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckConfig(ctx context.Context, targetType elbv2model.TargetType) (*elbv2model.TargetGroupHealthCheckConfig, error) {
	if targetType == elbv2model.TargetTypeInstance && t.service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal &&
		t.service.Spec.Type == corev1.ServiceTypeLoadBalancer {
		return t.buildTargetGroupHealthCheckConfigForInstanceModeLocal(ctx)
	}
	return t.buildTargetGroupHealthCheckConfigDefault(ctx)
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckConfigDefault(ctx context.Context) (*elbv2model.TargetGroupHealthCheckConfig, error) {
	healthCheckProtocol, err := t.buildTargetGroupHealthCheckProtocol(ctx, t.defaultHealthCheckProtocol)
	if err != nil {
		return nil, err
	}
	var healthCheckPathPtr *string
	if healthCheckProtocol != elbv2model.ProtocolTCP {
		healthCheckPathPtr = t.buildTargetGroupHealthCheckPath(ctx, t.defaultHealthCheckPath)
	}
	healthCheckPort, err := t.buildTargetGroupHealthCheckPort(ctx, t.defaultHealthCheckPort)
	if err != nil {
		return nil, err
	}
	intervalSeconds, err := t.buildTargetGroupHealthCheckIntervalSeconds(ctx, t.defaultHealthCheckInterval)
	if err != nil {
		return nil, err
	}
	healthyThresholdCount, err := t.buildTargetGroupHealthCheckHealthyThresholdCount(ctx, t.defaultHealthCheckHealthyThreshold)
	if err != nil {
		return nil, err
	}
	unhealthyThresholdCount, err := t.buildTargetGroupHealthCheckUnhealthyThresholdCount(ctx, t.defaultHealthCheckUnhealthyThreshold)
	if err != nil {
		return nil, err
	}
	return &elbv2model.TargetGroupHealthCheckConfig{
		Port:                    &healthCheckPort,
		Protocol:                &healthCheckProtocol,
		Path:                    healthCheckPathPtr,
		IntervalSeconds:         &intervalSeconds,
		HealthyThresholdCount:   &healthyThresholdCount,
		UnhealthyThresholdCount: &unhealthyThresholdCount,
	}, nil
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckConfigForInstanceModeLocal(ctx context.Context) (*elbv2model.TargetGroupHealthCheckConfig, error) {
	healthCheckProtocol, err := t.buildTargetGroupHealthCheckProtocol(ctx, t.defaultHealthCheckProtocolForInstanceModeLocal)
	if err != nil {
		return nil, err
	}
	var healthCheckPathPtr *string
	if healthCheckProtocol != elbv2model.ProtocolTCP {
		healthCheckPathPtr = t.buildTargetGroupHealthCheckPath(ctx, t.defaultHealthCheckPathForInstanceModeLocal)
	}
	healthCheckPort, err := t.buildTargetGroupHealthCheckPort(ctx, t.defaultHealthCheckPortForInstanceModeLocal)
	if err != nil {
		return nil, err
	}
	intervalSeconds, err := t.buildTargetGroupHealthCheckIntervalSeconds(ctx, t.defaultHealthCheckIntervalForInstanceModeLocal)
	if err != nil {
		return nil, err
	}
	healthyThresholdCount, err := t.buildTargetGroupHealthCheckHealthyThresholdCount(ctx, t.defaultHealthCheckHealthyThresholdForInstanceModeLocal)
	if err != nil {
		return nil, err
	}
	unhealthyThresholdCount, err := t.buildTargetGroupHealthCheckUnhealthyThresholdCount(ctx, t.defaultHealthCheckUnhealthyThresholdForInstanceModeLocal)
	if err != nil {
		return nil, err
	}
	return &elbv2model.TargetGroupHealthCheckConfig{
		Port:                    &healthCheckPort,
		Protocol:                &healthCheckProtocol,
		Path:                    healthCheckPathPtr,
		IntervalSeconds:         &intervalSeconds,
		HealthyThresholdCount:   &healthyThresholdCount,
		UnhealthyThresholdCount: &unhealthyThresholdCount,
	}, nil
}

var invalidTargetGroupNamePattern = regexp.MustCompile("[[:^alnum:]]")

func (t *defaultModelBuildTask) buildTargetGroupName(_ context.Context, svcPort intstr.IntOrString, tgPort int64,
	targetType elbv2model.TargetType, tgProtocol elbv2model.Protocol, hc *elbv2model.TargetGroupHealthCheckConfig) string {
	healthCheckProtocol := string(elbv2model.ProtocolTCP)
	healthCheckInterval := strconv.FormatInt(t.defaultHealthCheckInterval, 10)
	if hc.Protocol != nil {
		healthCheckProtocol = string(*hc.Protocol)
	}
	if hc.IntervalSeconds != nil {
		healthCheckInterval = strconv.FormatInt(*hc.IntervalSeconds, 10)
	}
	uuidHash := sha256.New()
	_, _ = uuidHash.Write([]byte(t.clusterName))
	_, _ = uuidHash.Write([]byte(t.service.UID))
	_, _ = uuidHash.Write([]byte(strconv.Itoa(int(tgPort))))
	_, _ = uuidHash.Write([]byte(svcPort.String()))
	_, _ = uuidHash.Write([]byte(targetType))
	_, _ = uuidHash.Write([]byte(tgProtocol))
	_, _ = uuidHash.Write([]byte(healthCheckProtocol))
	_, _ = uuidHash.Write([]byte(healthCheckInterval))
	uuid := hex.EncodeToString(uuidHash.Sum(nil))

	sanitizedNamespace := invalidTargetGroupNamePattern.ReplaceAllString(t.service.Namespace, "")
	sanitizedName := invalidTargetGroupNamePattern.ReplaceAllString(t.service.Name, "")
	return fmt.Sprintf("k8s-%.8s-%.8s-%.10s", sanitizedNamespace, sanitizedName, uuid)
}

func (t *defaultModelBuildTask) buildTargetGroupAttributes(_ context.Context) ([]elbv2model.TargetGroupAttribute, error) {
	var rawAttributes map[string]string
	if _, err := t.annotationParser.ParseStringMapAnnotation(annotations.SvcLBSuffixTargetGroupAttributes, &rawAttributes, t.service.Annotations); err != nil {
		return nil, err
	}
	if rawAttributes == nil {
		rawAttributes = make(map[string]string)
	}
	if _, ok := rawAttributes[tgAttrsProxyProtocolV2Enabled]; !ok {
		rawAttributes[tgAttrsProxyProtocolV2Enabled] = strconv.FormatBool(t.defaultProxyProtocolV2Enabled)
	}
	proxyV2Annotation := ""
	if exists := t.annotationParser.ParseStringAnnotation(annotations.SvcLBSuffixProxyProtocol, &proxyV2Annotation, t.service.Annotations); exists {
		if proxyV2Annotation != "*" {
			return []elbv2model.TargetGroupAttribute{}, errors.Errorf("invalid value %v for Load Balancer proxy protocol v2 annotation, only value currently supported is *", proxyV2Annotation)
		}
		rawAttributes[tgAttrsProxyProtocolV2Enabled] = "true"
	}
	if rawPreserveIPEnabled, ok := rawAttributes[tgAttrsPreserveClientIPEnabled]; ok {
		_, err := strconv.ParseBool(rawPreserveIPEnabled)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse attribute %v=%v", tgAttrsPreserveClientIPEnabled, rawPreserveIPEnabled)
		}
	}
	attributes := make([]elbv2model.TargetGroupAttribute, 0, len(rawAttributes))
	for attrKey, attrValue := range rawAttributes {
		attributes = append(attributes, elbv2model.TargetGroupAttribute{
			Key:   attrKey,
			Value: attrValue,
		})
	}
	sort.Slice(attributes, func(i, j int) bool {
		return attributes[i].Key < attributes[j].Key
	})
	return attributes, nil
}

func (t *defaultModelBuildTask) buildPreserveClientIPFlag(_ context.Context, targetType elbv2model.TargetType, tgAttrs []elbv2model.TargetGroupAttribute) (bool, error) {
	for _, attr := range tgAttrs {
		if attr.Key == tgAttrsPreserveClientIPEnabled {
			preserveClientIP, err := strconv.ParseBool(attr.Value)
			if err != nil {
				return false, errors.Wrapf(err, "failed to parse attribute %v=%v", tgAttrsPreserveClientIPEnabled, attr.Value)
			}
			return preserveClientIP, nil
		}
	}
	switch targetType {
	case elbv2model.TargetTypeIP:
		return false, nil
	case elbv2model.TargetTypeInstance:
		return true, nil
	}
	return false, nil
}

// buildTargetGroupPort constructs the TargetGroup's port.
// Note: TargetGroup's port is not in the data path as we always register targets with port specified.
// so this settings don't really matter to our controller, and we do our best to use the most appropriate port as targetGroup's port to avoid UX confusing.
func (t *defaultModelBuildTask) buildTargetGroupPort(_ context.Context, targetType elbv2model.TargetType, svcPort corev1.ServicePort) int64 {
	if targetType == elbv2model.TargetTypeInstance {
		return int64(svcPort.NodePort)
	}
	if svcPort.TargetPort.Type == intstr.Int {
		return int64(svcPort.TargetPort.IntValue())
	}

	// when a literal targetPort is used, we just use a fixed 1 here as this setting is not in the data path.
	// also, under extreme edge case, it can actually be different ports for different pods.
	return 1
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckPort(_ context.Context, defaultHealthCheckPort string) (intstr.IntOrString, error) {
	rawHealthCheckPort := defaultHealthCheckPort
	t.annotationParser.ParseStringAnnotation(annotations.SvcLBSuffixHCPort, &rawHealthCheckPort, t.service.Annotations)
	if rawHealthCheckPort == healthCheckPortTrafficPort {
		return intstr.FromString(rawHealthCheckPort), nil
	}
	portVal, err := strconv.ParseInt(rawHealthCheckPort, 10, 64)
	if err != nil {
		return intstr.IntOrString{}, errors.Errorf("health check port \"%v\" not supported", rawHealthCheckPort)
	}
	return intstr.FromInt(int(portVal)), nil
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckProtocol(_ context.Context, defaultHealthCheckProtocol elbv2model.Protocol) (elbv2model.Protocol, error) {
	rawHealthCheckProtocol := string(defaultHealthCheckProtocol)
	t.annotationParser.ParseStringAnnotation(annotations.SvcLBSuffixHCProtocol, &rawHealthCheckProtocol, t.service.Annotations)
	switch strings.ToUpper(rawHealthCheckProtocol) {
	case string(elbv2model.ProtocolTCP):
		return elbv2model.ProtocolTCP, nil
	case string(elbv2model.ProtocolHTTP):
		return elbv2model.ProtocolHTTP, nil
	case string(elbv2model.ProtocolHTTPS):
		return elbv2model.ProtocolHTTPS, nil
	default:
		return "", errors.Errorf("unsupported health check protocol %v", rawHealthCheckProtocol)
	}
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckPath(_ context.Context, defaultHealthCheckPath string) *string {
	healthCheckPath := defaultHealthCheckPath
	t.annotationParser.ParseStringAnnotation(annotations.SvcLBSuffixHCPath, &healthCheckPath, t.service.Annotations)
	return &healthCheckPath
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckIntervalSeconds(_ context.Context, defaultHealthCheckInterval int64) (int64, error) {
	intervalSeconds := defaultHealthCheckInterval
	if _, err := t.annotationParser.ParseInt64Annotation(annotations.SvcLBSuffixHCInterval, &intervalSeconds, t.service.Annotations); err != nil {
		return 0, err
	}
	return intervalSeconds, nil
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckTimeoutSeconds(_ context.Context, defaultHealthCheckTimeout int64) (int64, error) {
	timeoutSeconds := defaultHealthCheckTimeout
	if _, err := t.annotationParser.ParseInt64Annotation(annotations.SvcLBSuffixHCTimeout, &timeoutSeconds, t.service.Annotations); err != nil {
		return 0, err
	}
	return timeoutSeconds, nil
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckHealthyThresholdCount(_ context.Context, defaultHealthCheckHealthyThreshold int64) (int64, error) {
	healthyThresholdCount := defaultHealthCheckHealthyThreshold
	if _, err := t.annotationParser.ParseInt64Annotation(annotations.SvcLBSuffixHCHealthyThreshold, &healthyThresholdCount, t.service.Annotations); err != nil {
		return 0, err
	}
	return healthyThresholdCount, nil
}

func (t *defaultModelBuildTask) buildTargetGroupHealthCheckUnhealthyThresholdCount(_ context.Context, defaultHealthCheckUnhealthyThreshold int64) (int64, error) {
	unhealthyThresholdCount := defaultHealthCheckUnhealthyThreshold
	if _, err := t.annotationParser.ParseInt64Annotation(annotations.SvcLBSuffixHCUnhealthyThreshold, &unhealthyThresholdCount, t.service.Annotations); err != nil {
		return 0, err
	}
	return unhealthyThresholdCount, nil
}

func (t *defaultModelBuildTask) buildTargetType(_ context.Context) (elbv2model.TargetType, error) {
	var lbType string
	_ = t.annotationParser.ParseStringAnnotation(annotations.SvcLBSuffixLoadBalancerType, &lbType, t.service.Annotations)
	var lbTargetType string
	_ = t.annotationParser.ParseStringAnnotation(annotations.SvcLBSuffixTargetType, &lbTargetType, t.service.Annotations)
	if lbType == LoadBalancerTypeNLBIP || (lbType == LoadBalancerTypeExternal && lbTargetType == LoadBalancerTargetTypeIP) {
		return elbv2model.TargetTypeIP, nil
	}
	if lbType == LoadBalancerTypeExternal && lbTargetType == LoadBalancerTargetTypeInstance {
		return elbv2model.TargetTypeInstance, nil
	}
	return "", errors.Errorf("unsupported target type \"%v\" for load balancer type \"%v\"", lbTargetType, lbType)
}

func (t *defaultModelBuildTask) buildTargetGroupResourceID(svcKey types.NamespacedName, port intstr.IntOrString) string {
	return fmt.Sprintf("%s/%s:%s", svcKey.Namespace, svcKey.Name, port.String())
}

func (t *defaultModelBuildTask) buildTargetGroupTags(ctx context.Context) (map[string]string, error) {
	return t.buildAdditionalResourceTags(ctx)
}

func (t *defaultModelBuildTask) buildTargetGroupBinding(ctx context.Context, targetGroup *elbv2model.TargetGroup, preserveClientIP bool,
	port corev1.ServicePort, hc *elbv2model.TargetGroupHealthCheckConfig, scheme elbv2model.LoadBalancerScheme) (*elbv2model.TargetGroupBindingResource, error) {
	tgbSpec, err := t.buildTargetGroupBindingSpec(ctx, targetGroup, preserveClientIP, port, hc, scheme)
	if err != nil {
		return nil, err
	}
	return elbv2model.NewTargetGroupBindingResource(t.stack, targetGroup.ID(), tgbSpec), nil
}

func (t *defaultModelBuildTask) buildTargetGroupBindingSpec(ctx context.Context, targetGroup *elbv2model.TargetGroup, preserveClientIP bool,
	port corev1.ServicePort, hc *elbv2model.TargetGroupHealthCheckConfig, scheme elbv2model.LoadBalancerScheme) (elbv2model.TargetGroupBindingResourceSpec, error) {
	nodeSelector, err := t.buildTargetGroupBindingNodeSelector(ctx, targetGroup.Spec.TargetType)
	if err != nil {
		return elbv2model.TargetGroupBindingResourceSpec{}, err
	}
	// buildTargetGroupBindingNetwork() will try to find the target Pod networking information,
	// in order to adjust the node security group to allow connections from the new load balancer
	// towards the target Pods. However, this is only supported on the native AWS CNI, which we
	// don't use, so it will fail.
	// In our case, we don't need to ajust the node security group, so we skip this step altogether
	// and supply a nil networking requirement.
	/*
	targetPort := port.TargetPort
	targetType := elbv2api.TargetType(targetGroup.Spec.TargetType)
	if targetType == elbv2api.TargetTypeInstance {
		targetPort = intstr.FromInt(int(port.NodePort))
	}
	defaultSourceRanges := []string{"0.0.0.0/0"}
	if preserveClientIP && scheme == elbv2model.LoadBalancerSchemeInternal {
		defaultSourceRanges, err = t.vpcResolver.ResolveCIDRs(ctx)
		if err != nil {
			return elbv2model.TargetGroupBindingResourceSpec{}, err
		}
	}
	tgbNetworking := t.buildTargetGroupBindingNetworking(ctx, targetPort, preserveClientIP, *hc.Port, port.Protocol, defaultSourceRanges)
	*/
	targetType := elbv2api.TargetType(targetGroup.Spec.TargetType)
	var tgbNetworking *elbv2model.TargetGroupBindingNetworking
	return elbv2model.TargetGroupBindingResourceSpec{
		Template: elbv2model.TargetGroupBindingTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: t.service.Namespace,
				Name:      targetGroup.Spec.Name,
			},
			Spec: elbv2model.TargetGroupBindingSpec{
				TargetGroupARN: targetGroup.TargetGroupARN(),
				TargetType:     &targetType,
				ServiceRef: elbv2api.ServiceReference{
					Name: t.service.Name,
					Port: intstr.FromInt(int(port.Port)),
				},
				Networking:   tgbNetworking,
				NodeSelector: nodeSelector,
			},
		},
	}, nil
}

func (t *defaultModelBuildTask) buildPeersFromSourceRanges(_ context.Context, defaultSourceRanges []string) []elbv2model.NetworkingPeer {
	var sourceRanges []string
	var peers []elbv2model.NetworkingPeer
	for _, cidr := range t.service.Spec.LoadBalancerSourceRanges {
		sourceRanges = append(sourceRanges, cidr)
	}
	if len(sourceRanges) == 0 {
		t.annotationParser.ParseStringSliceAnnotation(annotations.SvcLBSuffixSourceRanges, &sourceRanges, t.service.Annotations)
	}
	if len(sourceRanges) == 0 {
		sourceRanges = defaultSourceRanges
	}
	for _, cidr := range sourceRanges {
		peers = append(peers, elbv2model.NetworkingPeer{
			IPBlock: &elbv2api.IPBlock{
				CIDR: cidr,
			},
		})
	}
	return peers
}

func (t *defaultModelBuildTask) buildTargetGroupBindingNetworking(ctx context.Context, tgPort intstr.IntOrString, preserveClientIP bool,
	hcPort intstr.IntOrString, tgProtocol corev1.Protocol, defaultSourceRanges []string) *elbv2model.TargetGroupBindingNetworking {
	var fromVPC []elbv2model.NetworkingPeer
	for _, subnet := range t.ec2Subnets {
		fromVPC = append(fromVPC, elbv2model.NetworkingPeer{
			IPBlock: &elbv2api.IPBlock{
				CIDR: aws.StringValue(subnet.CidrBlock),
			},
		})
	}
	networkingProtocol := elbv2api.NetworkingProtocolTCP
	if tgProtocol == corev1.ProtocolUDP {
		networkingProtocol = elbv2api.NetworkingProtocolUDP
	}
	trafficPorts := []elbv2api.NetworkingPort{
		{
			Port:     &tgPort,
			Protocol: &networkingProtocol,
		},
	}
	trafficSource := fromVPC
	if networkingProtocol == elbv2api.NetworkingProtocolUDP || preserveClientIP {
		trafficSource = t.buildPeersFromSourceRanges(ctx, defaultSourceRanges)
	}
	tgbNetworking := &elbv2model.TargetGroupBindingNetworking{
		Ingress: []elbv2model.NetworkingIngressRule{
			{
				From:  trafficSource,
				Ports: trafficPorts,
			},
		},
	}
	if preserveClientIP || tgProtocol == corev1.ProtocolUDP || (hcPort.String() != healthCheckPortTrafficPort && hcPort.IntValue() != tgPort.IntValue()) {
		var healthCheckPorts []elbv2api.NetworkingPort
		networkingProtocolTCP := elbv2api.NetworkingProtocolTCP
		networkingHealthCheckPort := hcPort
		if hcPort.String() == healthCheckPortTrafficPort {
			networkingHealthCheckPort = tgPort
		}
		healthCheckPorts = append(healthCheckPorts, elbv2api.NetworkingPort{
			Port:     &networkingHealthCheckPort,
			Protocol: &networkingProtocolTCP,
		})
		tgbNetworking.Ingress = append(tgbNetworking.Ingress, elbv2model.NetworkingIngressRule{
			From:  fromVPC,
			Ports: healthCheckPorts,
		})
	}
	return tgbNetworking
}

func (t *defaultModelBuildTask) buildTargetGroupBindingNodeSelector(_ context.Context, targetType elbv2model.TargetType) (*metav1.LabelSelector, error) {
	if targetType != elbv2model.TargetTypeInstance {
		return nil, nil
	}
	var targetNodeLabels map[string]string
	if _, err := t.annotationParser.ParseStringMapAnnotation(annotations.SvcLBSuffixTargetNodeLabels, &targetNodeLabels, t.service.Annotations); err != nil {
		return nil, err
	}
	if len(targetNodeLabels) == 0 {
		return nil, nil
	}
	return &metav1.LabelSelector{
		MatchLabels: targetNodeLabels,
	}, nil
}
