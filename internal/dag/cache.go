// Copyright Project Contour Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dag

import (
	"errors"
	"fmt"
	"sync"

	contour_api_v1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	contour_api_v1alpha1 "github.com/projectcontour/contour/apis/projectcontour/v1alpha1"
	"github.com/projectcontour/contour/internal/annotation"
	"github.com/projectcontour/contour/internal/k8s"
	"github.com/sirupsen/logrus"
	core_v1 "k8s.io/api/core/v1"
	networking_v1 "k8s.io/api/networking/v1"
	"k8s.io/api/networking/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	gatewayapi_v1alpha1 "sigs.k8s.io/gateway-api/apis/v1alpha1"
)

// A KubernetesCache holds Kubernetes objects and associated configuration and produces
// DAG values.
type KubernetesCache struct {
	// RootNamespaces specifies the namespaces where root
	// HTTPProxies can be defined. If empty, roots can be defined in any
	// namespace.
	RootNamespaces []string

	// Contour's IngressClass.
	// If not set, defaults to DEFAULT_INGRESS_CLASS.
	IngressClass string

	// Gateway defines the current Gateway which Contour is configured to watch.
	Gateway types.NamespacedName

	// Secrets that are referred from the configuration file.
	ConfiguredSecretRefs []*types.NamespacedName

	ingresses            map[types.NamespacedName]*networking_v1.Ingress
	ingressclasses       map[string]*networking_v1.IngressClass
	httpproxies          map[types.NamespacedName]*contour_api_v1.HTTPProxy
	secrets              map[types.NamespacedName]*core_v1.Secret
	httpproxydelegations map[types.NamespacedName]*contour_api_v1.TLSCertificateDelegation
	services             map[types.NamespacedName]*core_v1.Service
	namespaces           map[types.NamespacedName]*core_v1.Namespace
	gateway              *gatewayapi_v1alpha1.Gateway
	httproutes           map[types.NamespacedName]*gatewayapi_v1alpha1.HTTPRoute
	tlsroutes            map[types.NamespacedName]*gatewayapi_v1alpha1.TLSRoute
	backendpolicies      map[types.NamespacedName]*gatewayapi_v1alpha1.BackendPolicy
	extensions           map[types.NamespacedName]*contour_api_v1alpha1.ExtensionService

	initialize sync.Once

	logrus.FieldLogger
}

// init creates the internal cache storage. It is called implicitly from the public API.
func (kc *KubernetesCache) init() {
	kc.ingresses = make(map[types.NamespacedName]*networking_v1.Ingress)
	kc.ingressclasses = make(map[string]*networking_v1.IngressClass)
	kc.httpproxies = make(map[types.NamespacedName]*contour_api_v1.HTTPProxy)
	kc.secrets = make(map[types.NamespacedName]*core_v1.Secret)
	kc.httpproxydelegations = make(map[types.NamespacedName]*contour_api_v1.TLSCertificateDelegation)
	kc.services = make(map[types.NamespacedName]*core_v1.Service)
	kc.namespaces = make(map[types.NamespacedName]*core_v1.Namespace)
	kc.httproutes = make(map[types.NamespacedName]*gatewayapi_v1alpha1.HTTPRoute)
	kc.tlsroutes = make(map[types.NamespacedName]*gatewayapi_v1alpha1.TLSRoute)
	kc.backendpolicies = make(map[types.NamespacedName]*gatewayapi_v1alpha1.BackendPolicy)
	kc.extensions = make(map[types.NamespacedName]*contour_api_v1alpha1.ExtensionService)
}

// matchesIngressClass returns true if the given Kubernetes object
// belongs to the Ingress class that this cache is using.
func (kc *KubernetesCache) matchesIngressClass(obj meta_v1.Object) bool {

	if !annotation.MatchesIngressClass(obj, kc.IngressClass) {
		kind := k8s.KindOf(obj)

		kc.WithField("name", obj.GetName()).
			WithField("namespace", obj.GetNamespace()).
			WithField("kind", kind).
			WithField("ingress-class", annotation.IngressClass(obj)).
			WithField("target-ingress-class", kc.IngressClass).
			Debug("ignoring object with unmatched ingress class")
		return false
	}

	return true

}

// matchesGateway returns true if the given Kubernetes object
// belongs to the Gateway that this cache is using.
func (kc *KubernetesCache) matchesGateway(obj *gatewayapi_v1alpha1.Gateway) bool {

	if k8s.NamespacedNameOf(obj) != kc.Gateway {
		kind := k8s.KindOf(obj)

		kc.WithField("name", obj.GetName()).
			WithField("namespace", obj.GetNamespace()).
			WithField("kind", kind).
			WithField("configured gateway name", kc.Gateway.Name).
			WithField("configured gateway namespace", kc.Gateway.Namespace).
			Debug("ignoring object with unmatched gateway")
		return false
	}
	return true
}

// Insert inserts obj into the KubernetesCache.
// Insert returns true if the cache accepted the object, or false if the value
// is not interesting to the cache. If an object with a matching type, name,
// and namespace exists, it will be overwritten.
func (kc *KubernetesCache) Insert(obj interface{}) bool {
	kc.initialize.Do(kc.init)

	if obj, ok := obj.(meta_v1.Object); ok {
		kind := k8s.KindOf(obj)
		for key := range obj.GetAnnotations() {
			// Emit a warning if this is a known annotation that has
			// been applied to an invalid object kind. Note that we
			// only warn for known annotations because we want to
			// allow users to add arbitrary orthogonal annotations
			// to objects that we inspect.
			if annotation.IsKnown(key) && !annotation.ValidForKind(kind, key) {
				// TODO(jpeach): this should be exposed
				// to the user as a status condition.
				kc.WithField("name", obj.GetName()).
					WithField("namespace", obj.GetNamespace()).
					WithField("kind", kind).
					WithField("version", k8s.VersionOf(obj)).
					WithField("annotation", key).
					Error("ignoring invalid or unsupported annotation")
			}
		}
	}

	switch obj := obj.(type) {
	case *core_v1.Secret:
		valid, err := isValidSecret(obj)
		if !valid {
			if err != nil {
				kc.WithField("name", obj.GetName()).
					WithField("namespace", obj.GetNamespace()).
					WithField("kind", "Secret").
					WithField("version", k8s.VersionOf(obj)).
					Error(err)
			}
			return false
		}

		kc.secrets[k8s.NamespacedNameOf(obj)] = obj
		return kc.secretTriggersRebuild(obj)
	case *core_v1.Service:
		kc.services[k8s.NamespacedNameOf(obj)] = obj
		return kc.serviceTriggersRebuild(obj)
	case *core_v1.Namespace:
		kc.namespaces[k8s.NamespacedNameOf(obj)] = obj
		return true
	case *v1beta1.Ingress:
		if kc.matchesIngressClass(obj) {
			// Convert the v1beta1 object to v1 before adding to the
			// local ingress cache for easier processing later on.
			kc.ingresses[k8s.NamespacedNameOf(obj)] = toV1Ingress(obj)
			return true
		}
	case *networking_v1.Ingress:
		if kc.matchesIngressClass(obj) {
			kc.ingresses[k8s.NamespacedNameOf(obj)] = obj
			return true
		}
	case *networking_v1.IngressClass:
		kc.ingressclasses[obj.Name] = obj
		return true
	case *contour_api_v1.HTTPProxy:
		if kc.matchesIngressClass(obj) {
			kc.httpproxies[k8s.NamespacedNameOf(obj)] = obj
			return true
		}
	case *contour_api_v1.TLSCertificateDelegation:
		kc.httpproxydelegations[k8s.NamespacedNameOf(obj)] = obj
		return true
	case *gatewayapi_v1alpha1.Gateway:
		if kc.matchesGateway(obj) {
			kc.WithField("experimental", "gateway-api").WithField("name", obj.Name).WithField("namespace", obj.Namespace).Debug("Adding Gateway")
			kc.gateway = obj
			return true
		}
	case *gatewayapi_v1alpha1.HTTPRoute:
		m := k8s.NamespacedNameOf(obj)
		kc.WithField("experimental", "gateway-api").WithField("name", m.Name).WithField("namespace", m.Namespace).Debug("Adding HTTPRoute")
		kc.httproutes[k8s.NamespacedNameOf(obj)] = obj
		return true
	case *gatewayapi_v1alpha1.TLSRoute:
		m := k8s.NamespacedNameOf(obj)
		// TODO(youngnick): Remove this once gateway-api actually have behavior
		// other than being added to the cache.
		kc.WithField("experimental", "gateway-api").WithField("name", m.Name).WithField("namespace", m.Namespace).Debug("Adding TLSRoute")
		kc.tlsroutes[k8s.NamespacedNameOf(obj)] = obj
		return true
	case *gatewayapi_v1alpha1.BackendPolicy:
		m := k8s.NamespacedNameOf(obj)
		// TODO(youngnick): Remove this once gateway-api actually have behavior
		// other than being added to the cache.
		kc.WithField("experimental", "gateway-api").WithField("name", m.Name).WithField("namespace", m.Namespace).Debug("Adding BackendPolicy")
		kc.backendpolicies[k8s.NamespacedNameOf(obj)] = obj
		return true
	case *contour_api_v1alpha1.ExtensionService:
		kc.extensions[k8s.NamespacedNameOf(obj)] = obj
		return true

	default:
		// not an interesting object
		kc.WithField("object", obj).Error("insert unknown object")
		return false
	}

	return false
}

func toV1Ingress(obj *v1beta1.Ingress) *networking_v1.Ingress {

	if obj == nil {
		return nil
	}

	var convertedTLS []networking_v1.IngressTLS
	var convertedIngressRules []networking_v1.IngressRule
	var paths []networking_v1.HTTPIngressPath
	var convertedDefaultBackend *networking_v1.IngressBackend

	for _, tls := range obj.Spec.TLS {
		convertedTLS = append(convertedTLS, networking_v1.IngressTLS{
			Hosts:      tls.Hosts,
			SecretName: tls.SecretName,
		})
	}

	for _, r := range obj.Spec.Rules {

		rule := networking_v1.IngressRule{}

		if r.Host != "" {
			rule.Host = r.Host
		}

		if r.HTTP != nil {

			for _, p := range r.HTTP.Paths {
				var pathType networking_v1.PathType
				if p.PathType != nil {
					switch *p.PathType {
					case v1beta1.PathTypePrefix:
						pathType = networking_v1.PathTypePrefix
					case v1beta1.PathTypeExact:
						pathType = networking_v1.PathTypeExact
					case v1beta1.PathTypeImplementationSpecific:
						pathType = networking_v1.PathTypeImplementationSpecific
					}
				}

				paths = append(paths, networking_v1.HTTPIngressPath{
					Path:     p.Path,
					PathType: &pathType,
					Backend: networking_v1.IngressBackend{
						Service: &networking_v1.IngressServiceBackend{
							Name: p.Backend.ServiceName,
							Port: serviceBackendPort(p.Backend.ServicePort),
						},
					},
				})
			}

			rule.IngressRuleValue = networking_v1.IngressRuleValue{
				HTTP: &networking_v1.HTTPIngressRuleValue{
					Paths: paths,
				},
			}
		}

		convertedIngressRules = append(convertedIngressRules, rule)
	}

	if obj.Spec.Backend != nil {
		convertedDefaultBackend = &networking_v1.IngressBackend{
			Service: &networking_v1.IngressServiceBackend{
				Name: obj.Spec.Backend.ServiceName,
				Port: serviceBackendPort(obj.Spec.Backend.ServicePort),
			},
		}
	}

	return &networking_v1.Ingress{
		ObjectMeta: obj.ObjectMeta,
		Spec: networking_v1.IngressSpec{
			DefaultBackend: convertedDefaultBackend,
			TLS:            convertedTLS,
			Rules:          convertedIngressRules,
		},
	}
}

func serviceBackendPort(port intstr.IntOrString) networking_v1.ServiceBackendPort {
	if port.Type == intstr.String {
		return networking_v1.ServiceBackendPort{
			Name: port.StrVal,
		}
	}
	return networking_v1.ServiceBackendPort{
		Number: port.IntVal,
	}
}

// Remove removes obj from the KubernetesCache.
// Remove returns a boolean indicating if the cache changed after the remove operation.
func (kc *KubernetesCache) Remove(obj interface{}) bool {
	kc.initialize.Do(kc.init)

	switch obj := obj.(type) {
	default:
		return kc.remove(obj)
	case cache.DeletedFinalStateUnknown:
		return kc.Remove(obj.Obj) // recurse into ourselves with the tombstoned value
	}
}

func (kc *KubernetesCache) remove(obj interface{}) bool {
	switch obj := obj.(type) {
	case *core_v1.Secret:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.secrets[m]
		delete(kc.secrets, m)
		return ok
	case *core_v1.Service:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.services[m]
		delete(kc.services, m)
		return ok
	case *core_v1.Namespace:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.namespaces[m]
		delete(kc.namespaces, m)
		return ok
	case *v1beta1.Ingress:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.ingresses[m]
		delete(kc.ingresses, m)
		return ok
	case *networking_v1.Ingress:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.ingresses[m]
		delete(kc.ingresses, m)
		return ok
	case *networking_v1.IngressClass:
		_, ok := kc.ingressclasses[obj.Name]
		delete(kc.ingressclasses, obj.Name)
		return ok
	case *contour_api_v1.HTTPProxy:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.httpproxies[m]
		delete(kc.httpproxies, m)
		return ok
	case *contour_api_v1.TLSCertificateDelegation:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.httpproxydelegations[m]
		delete(kc.httpproxydelegations, m)
		return ok
	case *gatewayapi_v1alpha1.Gateway:
		if kc.matchesGateway(obj) {
			kc.WithField("experimental", "gateway-api").WithField("name", obj.Name).WithField("namespace", obj.Namespace).Debug("Removing Gateway")
			kc.gateway = nil
			return true
		}
		return false
	case *gatewayapi_v1alpha1.HTTPRoute:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.httproutes[m]
		kc.WithField("experimental", "gateway-api").WithField("name", m.Name).WithField("namespace", m.Namespace).Debug("Removing HTTPRoute")
		delete(kc.httproutes, m)
		return ok
	case *gatewayapi_v1alpha1.TLSRoute:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.tlsroutes[m]
		// TODO(youngnick): Remove this once gateway-api actually have behavior
		// other than being removed from the cache.
		kc.WithField("experimental", "gateway-api").WithField("name", m.Name).WithField("namespace", m.Namespace).Debug("Removing TLSRoute")
		delete(kc.tlsroutes, m)
		return ok
	case *gatewayapi_v1alpha1.BackendPolicy:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.backendpolicies[m]
		// TODO(youngnick): Remove this once gateway-api actually have behavior
		// other than being removed from the cache.
		kc.WithField("experimental", "gateway-api").WithField("name", m.Name).WithField("namespace", m.Namespace).Debug("Removing BackendPolicy")
		delete(kc.backendpolicies, m)
		return ok
	case *contour_api_v1alpha1.ExtensionService:
		m := k8s.NamespacedNameOf(obj)
		_, ok := kc.extensions[m]
		delete(kc.extensions, m)
		return ok

	default:
		// not interesting
		kc.WithField("object", obj).Error("remove unknown object")
		return false
	}
}

// serviceTriggersRebuild returns true if this service is referenced
// by an Ingress or HTTPProxy in this cache.
func (kc *KubernetesCache) serviceTriggersRebuild(service *core_v1.Service) bool {
	for _, ingress := range kc.ingresses {
		if ingress.Namespace != service.Namespace {
			continue
		}
		if backend := ingress.Spec.DefaultBackend; backend != nil {
			if backend.Service.Name == service.Name {
				return true
			}
		}

		for _, rule := range ingress.Spec.Rules {
			http := rule.IngressRuleValue.HTTP
			if http == nil {
				continue
			}
			for _, path := range http.Paths {
				if path.Backend.Service.Name == service.Name {
					return true
				}
			}
		}
	}

	for _, ir := range kc.httpproxies {
		if ir.Namespace != service.Namespace {
			continue
		}
		for _, route := range ir.Spec.Routes {
			for _, s := range route.Services {
				if s.Name == service.Name {
					return true
				}
			}
		}
		if tcpproxy := ir.Spec.TCPProxy; tcpproxy != nil {
			for _, s := range tcpproxy.Services {
				if s.Name == service.Name {
					return true
				}
			}
		}
	}

	return false
}

// secretTriggersRebuild returns true if this secret is referenced by an Ingress
// or HTTPProxy object, or by the configuration file. If the secret is not in the same namespace
// it must be mentioned by a TLSCertificateDelegation.
func (kc *KubernetesCache) secretTriggersRebuild(secret *core_v1.Secret) bool {
	if _, isCA := secret.Data[CACertificateKey]; isCA {
		// locating a secret validation usage involves traversing each
		// proxy object, determining if there is a valid delegation,
		// and if the reference the secret as a certificate. The DAG already
		// does this so don't reproduce the logic and just assume for the moment
		// that any change to a CA secret will trigger a rebuild.
		return true
	}

	delegations := make(map[string]bool) // targetnamespace/secretname to bool

	// TODO(youngnick): Check if this is required.
	for _, d := range kc.httpproxydelegations {
		for _, cd := range d.Spec.Delegations {
			for _, n := range cd.TargetNamespaces {
				delegations[n+"/"+cd.SecretName] = true
			}
		}
	}

	for _, ingress := range kc.ingresses {
		if ingress.Namespace == secret.Namespace {
			for _, tls := range ingress.Spec.TLS {
				if tls.SecretName == secret.Name {
					return true
				}
			}
		}
		if delegations[ingress.Namespace+"/"+secret.Name] {
			for _, tls := range ingress.Spec.TLS {
				if tls.SecretName == secret.Namespace+"/"+secret.Name {
					return true
				}
			}
		}

		if delegations["*/"+secret.Name] {
			for _, tls := range ingress.Spec.TLS {
				if tls.SecretName == secret.Namespace+"/"+secret.Name {
					return true
				}
			}
		}
	}

	for _, proxy := range kc.httpproxies {
		vh := proxy.Spec.VirtualHost
		if vh == nil {
			// not a root ingress
			continue
		}
		tls := vh.TLS
		if tls == nil {
			// no tls spec
			continue
		}

		if proxy.Namespace == secret.Namespace && tls.SecretName == secret.Name {
			return true
		}
		if delegations[proxy.Namespace+"/"+secret.Name] {
			if tls.SecretName == secret.Namespace+"/"+secret.Name {
				return true
			}
		}
		if delegations["*/"+secret.Name] {
			if tls.SecretName == secret.Namespace+"/"+secret.Name {
				return true
			}
		}
	}

	// Secrets referred by the configuration file shall also trigger rebuild.
	for _, s := range kc.ConfiguredSecretRefs {
		if s.Namespace == secret.Namespace && s.Name == secret.Name {
			return true
		}
	}

	return false
}

// LookupSecret returns a Secret if present or nil if the underlying kubernetes
// secret fails validation or is missing.
func (kc *KubernetesCache) LookupSecret(name types.NamespacedName, validate func(*core_v1.Secret) error) (*Secret, error) {
	sec, ok := kc.secrets[name]
	if !ok {
		return nil, fmt.Errorf("Secret not found")
	}

	if err := validate(sec); err != nil {
		return nil, err
	}

	s := &Secret{
		Object: sec,
	}

	return s, nil
}

func (kc *KubernetesCache) LookupUpstreamValidation(uv *contour_api_v1.UpstreamValidation, namespace string) (*PeerValidationContext, error) {
	if uv == nil {
		// no upstream validation requested, nothing to do
		return nil, nil
	}

	secretName := types.NamespacedName{Name: uv.CACertificate, Namespace: namespace}
	cacert, err := kc.LookupSecret(secretName, validCA)
	if err != nil {
		// UpstreamValidation is requested, but cert is missing or not configured
		return nil, fmt.Errorf("invalid CA Secret %q: %s", secretName, err)
	}

	if uv.SubjectName == "" {
		// UpstreamValidation is requested, but SAN is not provided
		return nil, errors.New("missing subject alternative name")
	}

	return &PeerValidationContext{
		CACertificate: cacert,
		SubjectName:   uv.SubjectName,
	}, nil
}

func (kc *KubernetesCache) LookupDownstreamValidation(vc *contour_api_v1.DownstreamValidation, namespace string) (*PeerValidationContext, error) {
	secretName := types.NamespacedName{Name: vc.CACertificate, Namespace: namespace}
	cacert, err := kc.LookupSecret(secretName, validCA)
	if err != nil {
		// PeerValidationContext is requested, but cert is missing or not configured.
		return nil, fmt.Errorf("invalid CA Secret %q: %s", secretName, err)
	}

	return &PeerValidationContext{
		CACertificate: cacert,
	}, nil
}

// DelegationPermitted returns true if the referenced secret has been delegated
// to the namespace where the ingress object is located.
func (kc *KubernetesCache) DelegationPermitted(secret types.NamespacedName, targetNamespace string) bool {
	contains := func(haystack []string, needle string) bool {
		if len(haystack) == 1 && haystack[0] == "*" {
			return true
		}
		for _, h := range haystack {
			if h == needle {
				return true
			}
		}
		return false
	}

	if secret.Namespace == targetNamespace {
		// secret is in the same namespace as target
		return true
	}

	for _, d := range kc.httpproxydelegations {
		if d.Namespace != secret.Namespace {
			continue
		}
		for _, d := range d.Spec.Delegations {
			if contains(d.TargetNamespaces, targetNamespace) {
				if secret.Name == d.SecretName {
					return true
				}
			}
		}
	}
	return false
}

func validCA(s *core_v1.Secret) error {
	if len(s.Data[CACertificateKey]) == 0 {
		return fmt.Errorf("empty %q key", CACertificateKey)
	}

	return nil
}

// LookupService returns the Kubernetes service and port matching the provided parameters,
// or an error if a match can't be found.
func (kc *KubernetesCache) LookupService(meta types.NamespacedName, port intstr.IntOrString) (*core_v1.Service, core_v1.ServicePort, error) {
	svc, ok := kc.services[meta]
	if !ok {
		return nil, core_v1.ServicePort{}, fmt.Errorf("service %q not found", meta)
	}

	for i := range svc.Spec.Ports {
		p := svc.Spec.Ports[i]
		if int(p.Port) == port.IntValue() || port.String() == p.Name {
			switch p.Protocol {
			case "", core_v1.ProtocolTCP:
				return svc, p, nil
			default:
				return nil, core_v1.ServicePort{}, fmt.Errorf("unsupported service protocol %q", p.Protocol)
			}
		}
	}

	return nil, core_v1.ServicePort{}, fmt.Errorf("port %q on service %q not matched", port.String(), meta)
}
