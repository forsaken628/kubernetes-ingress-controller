package admission

import (
	"context"
	"fmt"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/kong/go-kong/kong"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/annotations"
	gatewaycontroller "github.com/kong/kubernetes-ingress-controller/v2/internal/controllers/gateway"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/dataplane/kongstate"
	credsvalidation "github.com/kong/kubernetes-ingress-controller/v2/internal/validation/consumers/credentials"
	gatewayvalidators "github.com/kong/kubernetes-ingress-controller/v2/internal/validation/gateway"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/versions"
	kongv1 "github.com/kong/kubernetes-ingress-controller/v2/pkg/apis/configuration/v1"
	kongv1beta1 "github.com/kong/kubernetes-ingress-controller/v2/pkg/apis/configuration/v1beta1"
)

// KongValidator validates Kong entities.
type KongValidator interface {
	ValidateConsumer(ctx context.Context, consumer kongv1.KongConsumer) (bool, string, error)
	ValidateConsumerGroup(ctx context.Context, consumerGroup kongv1beta1.KongConsumerGroup) (bool, string, error)
	ValidatePlugin(ctx context.Context, plugin kongv1.KongPlugin) (bool, string, error)
	ValidateClusterPlugin(ctx context.Context, plugin kongv1.KongClusterPlugin) (bool, string, error)
	ValidateCredential(ctx context.Context, secret corev1.Secret) (bool, string, error)
	ValidateGateway(ctx context.Context, gateway gatewaycontroller.Gateway) (bool, string, error)
	ValidateHTTPRoute(ctx context.Context, httproute gatewaycontroller.HTTPRoute) (bool, string, error)
}

// AdminAPIServicesProvider provides KongHTTPValidator with Kong Admin API services that are needed to perform
// validation against entities stored by the Gateway.
type AdminAPIServicesProvider interface {
	GetConsumersService() (kong.AbstractConsumerService, bool)
	GetPluginsService() (kong.AbstractPluginService, bool)
	GetConsumerGroupsService() (kong.AbstractConsumerGroupService, bool)
	GetInfoService() (kong.AbstractInfoService, bool)
}

// KongHTTPValidator implements KongValidator interface to validate Kong
// entities using the Admin API of Kong.
type KongHTTPValidator struct {
	Logger                   logrus.FieldLogger
	SecretGetter             kongstate.SecretGetter
	ManagerClient            client.Client
	AdminAPIServicesProvider AdminAPIServicesProvider

	ingressClassMatcher func(*metav1.ObjectMeta, string, annotations.ClassMatching) bool
}

// NewKongHTTPValidator provides a new KongHTTPValidator object provided a
// controller-runtime client which will be used to retrieve reference objects
// such as consumer credentials secrets. If you do not pass a cached client
// here, the performance of this validator can get very poor at high scales.
func NewKongHTTPValidator(
	logger logrus.FieldLogger,
	managerClient client.Client,
	ingressClass string,
	servicesProvider AdminAPIServicesProvider,
) KongHTTPValidator {
	matcher := annotations.IngressClassValidatorFuncFromObjectMeta(ingressClass)
	return KongHTTPValidator{
		Logger:                   logger,
		SecretGetter:             &managerClientSecretGetter{managerClient: managerClient},
		ManagerClient:            managerClient,
		AdminAPIServicesProvider: servicesProvider,

		ingressClassMatcher: matcher,
	}
}

// ValidateConsumer checks if consumer has a Username and a consumer with
// the same username doesn't exist in Kong.
// If an error occurs during validation, it is returned as the last argument.
// The first boolean communicates if the consumer is valid or not and string
// holds a message if the entity is not valid.
func (validator KongHTTPValidator) ValidateConsumer(
	ctx context.Context,
	consumer kongv1.KongConsumer,
) (bool, string, error) {
	// ignore consumers that are being managed by another controller
	if !validator.ingressClassMatcher(&consumer.ObjectMeta, annotations.IngressClassKey, annotations.ExactClassMatch) {
		return true, "", nil
	}

	// a consumer without a username is not valid
	if consumer.Username == "" {
		return false, ErrTextConsumerUsernameEmpty, nil
	}

	errText, err := validator.ensureConsumerDoesNotExistInGateway(ctx, consumer.Username)
	if err != nil || errText != "" {
		return false, errText, err
	}

	// if there are no credentials for this consumer, there's no need to move on
	// to credentials validation.
	if len(consumer.Credentials) == 0 {
		return true, "", nil
	}

	// pull all the managed consumers in order to build a validation index of
	// credentials so that the consumers credentials references can be validated.
	managedConsumers, err := validator.listManagedConsumers(ctx)
	if err != nil {
		return false, ErrTextConsumerUnretrievable, err
	}

	// retrieve the consumer's credentials secrets to validate them with the index
	credentials := make([]*corev1.Secret, 0, len(consumer.Credentials))
	ignoredSecrets := make(map[string]map[string]struct{})
	for _, secretName := range consumer.Credentials {
		// retrieve the credentials secret
		secret, err := validator.SecretGetter.GetSecret(consumer.Namespace, secretName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, ErrTextConsumerCredentialSecretNotFound, err
			}
			return false, ErrTextFailedToRetrieveSecret, err
		}

		// do the basic credentials validation
		if err := credsvalidation.ValidateCredentials(secret); err != nil {
			return false, ErrTextConsumerCredentialValidationFailed, err
		}

		// if valid, store it so we can index it for upcoming constraints validation
		credentials = append(credentials, secret)

		// later we'll build a global index of all credentials which is needed to
		// validate unique key constraints. That index should omit the secrets that
		// are referenced by this consumer to avoid duplication.
		if _, ok := ignoredSecrets[consumer.Namespace]; !ok {
			ignoredSecrets[consumer.Namespace] = make(map[string]struct{}, len(consumer.Credentials))
		}
		ignoredSecrets[consumer.Namespace][secretName] = struct{}{}
	}

	// unique constraints on consumer credentials are global to all consumers
	// and credentials, so we must build an index based on all existing credentials.
	// we ignore the secrets referenced by this consumer so that the index is not
	// testing them against themselves.
	credentialsIndex, err := globalValidationIndexForCredentials(ctx, validator.ManagerClient, managedConsumers, ignoredSecrets)
	if err != nil {
		return false, ErrTextConsumerCredentialValidationFailed, err
	}

	// validate the consumer's credentials against the index of all managed
	// credentials to ensure they're not in violation of any unique constraints.
	for _, secret := range credentials {
		// do the unique constraints validation of the credentials using the credentials index
		if err := credentialsIndex.ValidateCredentialsForUniqueKeyConstraints(secret); err != nil {
			return false, ErrTextConsumerCredentialValidationFailed, err
		}
	}

	return true, "", nil
}

func (validator KongHTTPValidator) ValidateConsumerGroup(
	ctx context.Context,
	consumerGroup kongv1beta1.KongConsumerGroup,
) (bool, string, error) {
	// Ignore ConsumerGroups that are being managed by another controller.
	if !validator.ingressClassMatcher(&consumerGroup.ObjectMeta, annotations.IngressClassKey, annotations.ExactClassMatch) {
		return true, "", nil
	}

	// Consumer groups work only for Kong Enterprise >=3.4.
	infoSvc, ok := validator.AdminAPIServicesProvider.GetInfoService()
	if !ok {
		return true, "", nil
	}
	info, err := infoSvc.Get(ctx)
	if err != nil {
		validator.Logger.Debugf("failed to fetch Kong info: %v", err)
		return false, ErrTextAdminAPIUnavailable, nil
	}
	version, err := kong.NewVersion(info.Version)
	if err != nil {
		validator.Logger.Debugf("failed to parse Kong version: %v", err)
	} else {
		kongVer := semver.Version{Major: version.Major(), Minor: version.Minor()}
		if !version.IsKongGatewayEnterprise() || !kongVer.GTE(versions.ConsumerGroupsVersionCutoff) {
			return false, ErrTextConsumerGroupUnsupported, nil
		}
	}

	cgs, ok := validator.AdminAPIServicesProvider.GetConsumerGroupsService()
	if !ok {
		return true, "", nil
	}
	// This check forbids consumer group creation if the license is invalid or missing.
	// There is no other way to robustly check the validity of a license than actually trying an enterprise feature.
	if _, _, err := cgs.List(ctx, &kong.ListOpt{Size: 0}); err != nil {
		switch {
		case kong.IsNotFoundErr(err):
			// This is the case when consumer group is not supported (Kong OSS) and previous version
			// check (if !version.IsKongGatewayEnterprise()) has been omitted due to a parsing error.
			return false, ErrTextConsumerGroupUnsupported, nil
		case kong.IsForbiddenErr(err):
			return false, ErrTextConsumerGroupUnlicensed, nil
		default:
			return false, fmt.Sprintf("%s: %s", ErrTextConsumerGroupUnexpected, err), nil
		}
	}
	return true, "", nil
}

// ValidateCredential checks if the secret contains a credential meant to
// be installed in Kong. If so, then it verifies if all the required fields
// are present in it or not. If valid, it returns true with an empty string,
// else it returns false with the error message. If an error happens during
// validation, error is returned.
func (validator KongHTTPValidator) ValidateCredential(
	ctx context.Context,
	secret corev1.Secret,
) (bool, string, error) {
	// if the secret doesn't contain a type key it's not a credentials secret
	_, ok := secret.Data[credsvalidation.TypeKey]
	if !ok {
		return true, "", nil
	}

	// credentials are only validated if they are referenced by a managed consumer
	// in the namespace, as such we pull a list of all consumers from the cached
	// client to determine if the credentials are referenced.
	managedConsumers, err := validator.listManagedConsumers(ctx)
	if err != nil {
		return false, ErrTextConsumerUnretrievable, err
	}

	// verify whether this secret is referenced by any managed consumer
	managedConsumersWithReferences := listManagedConsumersReferencingCredentialsSecret(secret, managedConsumers)
	if len(managedConsumersWithReferences) == 0 {
		// if no managed consumers reference this secret, its considered
		// unmanaged and we don't validate it unless it becomes referenced
		// by a managed consumer at a later time.
		return true, "", nil
	}

	// now that we know at least one managed consumer is referencing this
	// secret we perform the base-level credentials secret validation.
	if err := credsvalidation.ValidateCredentials(&secret); err != nil {
		return false, ErrTextConsumerCredentialValidationFailed, err
	}

	// if base-level validation passes we move on to create an index of
	// all managed credentials so that we can verify that the updates to
	// this secret are not in violation of any unique key constraints.
	ignoreSecrets := map[string]map[string]struct{}{secret.Namespace: {secret.Name: {}}}
	credentialsIndex, err := globalValidationIndexForCredentials(ctx, validator.ManagerClient, managedConsumers, ignoreSecrets)
	if err != nil {
		return false, ErrTextConsumerCredentialValidationFailed, err
	}

	// the index is built, now validate that the newly updated secret
	// is not in violation of any constraints.
	if err := credentialsIndex.ValidateCredentialsForUniqueKeyConstraints(&secret); err != nil {
		return false, ErrTextConsumerCredentialValidationFailed, err
	}

	return true, "", nil
}

// ValidatePlugin checks if k8sPlugin is valid. It does so by performing
// an HTTP request to Kong's Admin API entity validation endpoints.
// If an error occurs during validation, it is returned as the last argument.
// The first boolean communicates if k8sPluign is valid or not and string
// holds a message if the entity is not valid.
func (validator KongHTTPValidator) ValidatePlugin(
	ctx context.Context,
	k8sPlugin kongv1.KongPlugin,
) (bool, string, error) {
	if k8sPlugin.PluginName == "" {
		return false, ErrTextPluginNameEmpty, nil
	}
	var plugin kong.Plugin
	plugin.Name = kong.String(k8sPlugin.PluginName)
	var err error
	plugin.Config, err = kongstate.RawConfigToConfiguration(k8sPlugin.Config)
	if err != nil {
		return false, ErrTextPluginConfigInvalid, err
	}
	if k8sPlugin.ConfigFrom != nil {
		if len(plugin.Config) > 0 {
			return false, ErrTextPluginUsesBothConfigTypes, nil
		}
		config, err := kongstate.SecretToConfiguration(validator.SecretGetter, (*k8sPlugin.ConfigFrom).SecretValue, k8sPlugin.Namespace)
		if err != nil {
			return false, ErrTextPluginSecretConfigUnretrievable, err
		}
		plugin.Config = config
	}
	if k8sPlugin.RunOn != "" {
		plugin.RunOn = kong.String(k8sPlugin.RunOn)
	}
	if k8sPlugin.Ordering != nil {
		plugin.Ordering = k8sPlugin.Ordering
	}
	if len(k8sPlugin.Protocols) > 0 {
		plugin.Protocols = kong.StringSlice(kongv1.KongProtocolsToStrings(k8sPlugin.Protocols)...)
	}
	errText, err := validator.validatePluginAgainstGatewaySchema(ctx, plugin)
	if err != nil || errText != "" {
		return false, errText, err
	}

	return true, "", nil
}

// ValidateClusterPlugin transfers relevant fields from a KongClusterPlugin into a KongPlugin and then returns
// the result of ValidatePlugin for the derived KongPlugin.
func (validator KongHTTPValidator) ValidateClusterPlugin(
	ctx context.Context,
	k8sPlugin kongv1.KongClusterPlugin,
) (bool, string, error) {
	derived := kongv1.KongPlugin{
		TypeMeta:    k8sPlugin.TypeMeta,
		ObjectMeta:  k8sPlugin.ObjectMeta,
		ConsumerRef: k8sPlugin.ConsumerRef,
		Disabled:    k8sPlugin.Disabled,
		Config:      k8sPlugin.Config,
		PluginName:  k8sPlugin.PluginName,
		RunOn:       k8sPlugin.RunOn,
		Protocols:   k8sPlugin.Protocols,
	}
	if k8sPlugin.ConfigFrom != nil {
		ref := kongv1.ConfigSource{
			SecretValue: kongv1.SecretValueFromSource{
				Secret: k8sPlugin.ConfigFrom.SecretValue.Secret,
				Key:    k8sPlugin.ConfigFrom.SecretValue.Key,
			},
		}
		derived.ConfigFrom = &ref
		derived.ObjectMeta.Namespace = k8sPlugin.ConfigFrom.SecretValue.Namespace
	} else {
		derived.ObjectMeta.Namespace = "default"
	}
	return validator.ValidatePlugin(ctx, derived)
}

func (validator KongHTTPValidator) ValidateGateway(
	ctx context.Context, gateway gatewaycontroller.Gateway,
) (bool, string, error) {
	// check if the gateway declares a gateway class
	if gateway.Spec.GatewayClassName == "" {
		return true, "", nil
	}

	// validate the gatewayclass reference
	gwc := gatewaycontroller.GatewayClass{}
	if err := validator.ManagerClient.Get(ctx, client.ObjectKey{Name: string(gateway.Spec.GatewayClassName)}, &gwc); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return true, "", nil // not managed by this controller
		}
		return false, ErrTextCantRetrieveGatewayClass, err
	}

	// validate whether the gatewayclass is a supported class, if not
	// then this gateway belongs to another controller.
	if gwc.Spec.ControllerName != gatewaycontroller.GetControllerName() {
		return true, "", nil
	}

	return true, "", nil
}

func (validator KongHTTPValidator) ValidateHTTPRoute(
	ctx context.Context, httproute gatewaycontroller.HTTPRoute,
) (bool, string, error) {
	// in order to be sure whether or not an HTTPRoute resource is managed by this
	// controller we disallow references to Gateway resources that do not exist.
	var managedGateways []*gatewaycontroller.Gateway
	for _, parentRef := range httproute.Spec.ParentRefs {
		// determine the namespace of the gateway referenced via parentRef. If no
		// explicit namespace is provided, assume the namespace of the route.
		namespace := httproute.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		// gather the Gateway resource referenced by parentRef and fail validation
		// if there is no such Gateway resource.
		gateway := gatewaycontroller.Gateway{}
		if err := validator.ManagerClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      string(parentRef.Name),
		}, &gateway); err != nil {
			return false, fmt.Sprintf("couldn't retrieve referenced gateway %s/%s", namespace, parentRef.Name), err
		}

		// pull the referenced GatewayClass object from the Gateway
		gatewayClass := gatewaycontroller.GatewayClass{}
		if err := validator.ManagerClient.Get(ctx, client.ObjectKey{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
			return false, fmt.Sprintf("couldn't retrieve referenced gatewayclass %s", gateway.Spec.GatewayClassName), err
		}

		// determine ultimately whether the Gateway is managed by this controller implementation
		if gatewayClass.Spec.ControllerName == gatewaycontroller.GetControllerName() {
			managedGateways = append(managedGateways, &gateway)
		}
	}

	// if there are no managed Gateways this is not a supported HTTPRoute
	if len(managedGateways) == 0 {
		return true, "", nil
	}

	// now that we know whether or not the HTTPRoute is linked to a managed
	// Gateway we can run it through full validation.
	return gatewayvalidators.ValidateHTTPRoute(&httproute, managedGateways...)
}

// -----------------------------------------------------------------------------
// KongHTTPValidator - Private Methods
// -----------------------------------------------------------------------------

func (validator KongHTTPValidator) listManagedConsumers(ctx context.Context) ([]*kongv1.KongConsumer, error) {
	// gather a list of all consumers from the cached client
	consumers := &kongv1.KongConsumerList{}
	if err := validator.ManagerClient.List(ctx, consumers, &client.ListOptions{
		Namespace: corev1.NamespaceAll,
	}); err != nil {
		return nil, err
	}

	// reduce the consumer set to consumers managed by this controller
	managedConsumers := make([]*kongv1.KongConsumer, 0)
	for _, consumer := range consumers.Items {
		consumer := consumer
		if !validator.ingressClassMatcher(&consumer.ObjectMeta, annotations.IngressClassKey,
			annotations.ExactClassMatch) {
			// ignore consumers (and subsequently secrets) that are managed by other controllers
			continue
		}
		consumerCopy := consumer
		managedConsumers = append(managedConsumers, &consumerCopy)
	}

	return managedConsumers, nil
}

func (validator KongHTTPValidator) ensureConsumerDoesNotExistInGateway(ctx context.Context, username string) (string, error) {
	if consumerSvc, hasClient := validator.AdminAPIServicesProvider.GetConsumersService(); hasClient {
		// verify that the consumer is not already present in the data-plane
		c, err := consumerSvc.Get(ctx, &username)
		if err != nil {
			if !kong.IsNotFoundErr(err) {
				validator.Logger.WithError(err).Error("failed to fetch consumer from kong")
				return ErrTextConsumerUnretrievable, err
			}
		}
		if c != nil {
			return ErrTextConsumerExists, nil
		}
	}

	// if there's no client, do not verify existence with data-plane as there's none available
	return "", nil
}

func (validator KongHTTPValidator) validatePluginAgainstGatewaySchema(ctx context.Context, plugin kong.Plugin) (string, error) {
	pluginService, hasClient := validator.AdminAPIServicesProvider.GetPluginsService()
	if hasClient {
		isValid, msg, err := pluginService.Validate(ctx, &plugin)
		if err != nil {
			return ErrTextPluginConfigValidationFailed, err
		}
		if !isValid {
			return fmt.Sprintf(ErrTextPluginConfigViolatesSchema, msg), nil
		}
	}

	// if there's no client, do not verify with data-plane as there's none available
	return "", nil
}

// -----------------------------------------------------------------------------
// Private - Manager Client Secret Getter
// -----------------------------------------------------------------------------

type managerClientSecretGetter struct {
	managerClient client.Client
}

func (m *managerClientSecretGetter) GetSecret(namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	return secret, m.managerClient.Get(context.Background(), client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}, secret)
}
