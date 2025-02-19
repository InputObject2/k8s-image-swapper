package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/alitto/pond"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/transports/alltransports"
	ctypes "github.com/containers/image/v5/types"
	"github.com/estahn/k8s-image-swapper/pkg/config"
	"github.com/estahn/k8s-image-swapper/pkg/registry"
	"github.com/estahn/k8s-image-swapper/pkg/secrets"
	types "github.com/estahn/k8s-image-swapper/pkg/types"
	jmespath "github.com/jmespath/go-jmespath"
	"github.com/rs/zerolog/log"
	kwhmodel "github.com/slok/kubewebhook/v2/pkg/model"
	"github.com/slok/kubewebhook/v2/pkg/webhook"
	kwhmutating "github.com/slok/kubewebhook/v2/pkg/webhook/mutating"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Option represents an option that can be passed when instantiating the image swapper to customize it
type Option func(*ImageSwapper)

// ImagePullSecretsProvider allows to pass a provider reading out Kubernetes secrets
func ImagePullSecretsProvider(provider secrets.ImagePullSecretsProvider) Option {
	return func(swapper *ImageSwapper) {
		swapper.imagePullSecretProvider = provider
	}
}

// Filters allows to pass JMESPathFilter to select the images to be swapped
func Filters(filters []config.JMESPathFilter) Option {
	return func(swapper *ImageSwapper) {
		swapper.filters = filters
	}
}

// ImageSwapPolicy allows to pass the ImageSwapPolicy option
func ImageSwapPolicy(policy types.ImageSwapPolicy) Option {
	return func(swapper *ImageSwapper) {
		swapper.imageSwapPolicy = policy
	}
}

// ImageCopyPolicy allows to pass the ImageCopyPolicy option
func ImageCopyPolicy(policy types.ImageCopyPolicy) Option {
	return func(swapper *ImageSwapper) {
		swapper.imageCopyPolicy = policy
	}
}

// ImageCopyDeadline allows to pass the ImageCopyPolicy option
func ImageCopyDeadline(deadline time.Duration) Option {
	return func(swapper *ImageSwapper) {
		swapper.imageCopyDeadline = deadline
	}
}

// Copier allows to pass the copier option
func Copier(pool *pond.WorkerPool) Option {
	return func(swapper *ImageSwapper) {
		swapper.copier = pool
	}
}

// ImageSwapper is a mutator that will download images and change the image name.
type ImageSwapper struct {
	sourceRegistryClients     []registry.Client
	destinationRegistryClient registry.Client
	imagePullSecretProvider   secrets.ImagePullSecretsProvider

	// filters defines a list of expressions to remove objects that should not be processed,
	// by default all objects will be processed
	filters []config.JMESPathFilter

	// copier manages the jobs copying the images to the target registry
	copier            *pond.WorkerPool
	imageCopyDeadline time.Duration

	imageSwapPolicy types.ImageSwapPolicy
	imageCopyPolicy types.ImageCopyPolicy
}

// NewImageSwapper returns a new ImageSwapper initialized.
func NewImageSwapper(sourceRegistryClients []registry.Client, registryClient registry.Client, imagePullSecretProvider secrets.ImagePullSecretsProvider, filters []config.JMESPathFilter, imageSwapPolicy types.ImageSwapPolicy, imageCopyPolicy types.ImageCopyPolicy, imageCopyDeadline time.Duration) kwhmutating.Mutator {
	return &ImageSwapper{
		sourceRegistryClients:     sourceRegistryClients,
		destinationRegistryClient: registryClient,
		imagePullSecretProvider:   imagePullSecretProvider,
		filters:                   filters,
		copier:                    pond.New(100, 1000),
		imageSwapPolicy:           imageSwapPolicy,
		imageCopyPolicy:           imageCopyPolicy,
		imageCopyDeadline:         imageCopyDeadline,
	}
}

// NewImageSwapperWithOpts returns a configured ImageSwapper instance
func NewImageSwapperWithOpts(sourceRegistryClient []registry.Client, destinationRegistryClient registry.Client, opts ...Option) kwhmutating.Mutator {
	swapper := &ImageSwapper{
		sourceRegistryClients:     sourceRegistryClient,
		destinationRegistryClient: destinationRegistryClient,
		imagePullSecretProvider:   secrets.NewDummyImagePullSecretsProvider(),
		filters:                   []config.JMESPathFilter{},
		imageSwapPolicy:           types.ImageSwapPolicyExists,
		imageCopyPolicy:           types.ImageCopyPolicyDelayed,
	}

	for _, opt := range opts {
		opt(swapper)
	}

	// Initialise worker pool if not configured
	if swapper.copier == nil {
		swapper.copier = pond.New(100, 1000)
	}

	return swapper
}

func NewImageSwapperWebhookWithOpts(sourceRegistryClient []registry.Client, destinationRegistryClient registry.Client, opts ...Option) (webhook.Webhook, error) {
	imageSwapper := NewImageSwapperWithOpts(sourceRegistryClient, destinationRegistryClient, opts...)
	mt := kwhmutating.MutatorFunc(imageSwapper.Mutate)
	mcfg := kwhmutating.WebhookConfig{
		ID:      "k8s-image-swapper",
		Obj:     &corev1.Pod{},
		Mutator: mt,
	}

	return kwhmutating.NewWebhook(mcfg)
}

func NewImageSwapperWebhook(sourceRegistryClients []registry.Client, registryClient registry.Client, imagePullSecretProvider secrets.ImagePullSecretsProvider, filters []config.JMESPathFilter, imageSwapPolicy types.ImageSwapPolicy, imageCopyPolicy types.ImageCopyPolicy, imageCopyDeadline time.Duration) (webhook.Webhook, error) {
	imageSwapper := NewImageSwapper(sourceRegistryClients, registryClient, imagePullSecretProvider, filters, imageSwapPolicy, imageCopyPolicy, imageCopyDeadline)
	mt := kwhmutating.MutatorFunc(imageSwapper.Mutate)
	mcfg := kwhmutating.WebhookConfig{
		ID:      "k8s-image-swapper",
		Obj:     &corev1.Pod{},
		Mutator: mt,
	}

	return kwhmutating.NewWebhook(mcfg)
}

// imageNamesWithDigestOrTag strips the tag from ambiguous image references that have a digest as well (e.g. `image:tag@sha256:123...`).
// Such image references are supported by docker but, due to their ambiguity,
// explicitly not by containers/image.
func imageNamesWithDigestOrTag(imageName string) (string, error) {
	ref, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return "", err
	}
	_, isTagged := ref.(reference.NamedTagged)
	canonical, isDigested := ref.(reference.Canonical)
	if isTagged && isDigested {
		canonical, err = reference.WithDigest(reference.TrimNamed(ref), canonical.Digest())
		if err != nil {
			return "", err
		}
		imageName = canonical.String()
	}
	return imageName, nil
}

// Mutate replaces the image ref. Satisfies mutating.Mutator interface.
func (p *ImageSwapper) Mutate(ctx context.Context, ar *kwhmodel.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return &kwhmutating.MutatorResult{}, nil
	}

	logger := log.With().
		Str("uid", string(ar.ID)).
		Str("kind", ar.RequestGVK.String()).
		Str("namespace", ar.Namespace).
		Str("name", pod.Name).
		Logger()

	lctx := logger.WithContext(context.Background())

	containerSets := []*[]corev1.Container{&pod.Spec.Containers, &pod.Spec.InitContainers}
	for _, containerSet := range containerSets {
		containers := *containerSet
		for i, container := range containers {

			srcRef, err := p.determineSrcRef(container, lctx)
			if err != nil {
				continue
			}

			// skip if the source originates from the target registry
			if p.destinationRegistryClient != nil && p.destinationRegistryClient.IsOrigin(srcRef) {
				log.Ctx(lctx).Debug().Str("registry", srcRef.DockerReference().String()).Msg("skip due to source and target being the same registry")
				continue
			}

			//skip if matches filter
			filterCtx := NewFilterContext(*ar, pod, container)
			if filterMatch(filterCtx, p.filters) {
				log.Ctx(lctx).Debug().Msg("skip due to filter condition")
				continue
			}

			targetRef := p.targetRef(srcRef)

			//perform copy
			p.copyImage(logger, lctx, pod, srcRef, targetRef, container)

			//swap container image
			p.swapContainerImage(lctx, targetRef, containers, i)

		}
	}

	return &kwhmutating.MutatorResult{MutatedObject: pod}, nil
}
func (p *ImageSwapper) determineSrcRef(container corev1.Container, lctx context.Context) (ctypes.ImageReference, error) {

	normalizedName, err := imageNamesWithDigestOrTag(container.Image)
	if err != nil {
		log.Ctx(lctx).Warn().Msgf("unable to normalize source name %s: %v", container.Image, err)
		return nil, err
	}

	srcRef, err := alltransports.ParseImageName("docker://" + normalizedName)
	if err != nil {
		log.Ctx(lctx).Warn().Msgf("invalid source name %s: %v", normalizedName, err)
		return nil, err
	}
	return srcRef, nil
}

func (p *ImageSwapper) copyImage(logger zerolog.Logger, lctx context.Context, pod *corev1.Pod,
	srcRef ctypes.ImageReference, targetRef ctypes.ImageReference, container corev1.Container) {

	targetImage := targetRef.DockerReference().String()
	imageCopierLogger := logger.With().
		Str("source-image", srcRef.DockerReference().String()).
		Str("target-image", targetImage).
		Logger()

	imageCopierContext := imageCopierLogger.WithContext(lctx)
	// create an object responsible for the image copy
	imageCopier := ImageCopier{
		sourcePod:       pod,
		sourceImageRef:  srcRef,
		targetImageRef:  targetRef,
		imagePullPolicy: container.ImagePullPolicy,
		imageSwapper:    p,
		context:         imageCopierContext,
	}

	// imageCopyPolicy
	switch p.imageCopyPolicy {
	case types.ImageCopyPolicyDelayed:
		p.copier.Submit(imageCopier.start)
	case types.ImageCopyPolicyImmediate:
		p.copier.SubmitAndWait(imageCopier.withDeadline().start)
	case types.ImageCopyPolicyForce:
		imageCopier.withDeadline().start()
	case types.ImageCopyPolicyNone:
		// do not copy image
	default:
		panic("unknown imageCopyPolicy")
	}
}
func (p *ImageSwapper) swapContainerImage(lctx context.Context, targetRef ctypes.ImageReference, containers []corev1.Container, i int) {

	targetImage := targetRef.DockerReference().String()

	switch p.imageSwapPolicy {
	case types.ImageSwapPolicyAlways:
		log.Ctx(lctx).Debug().Str("image", targetImage).Msg("set new container image")
		containers[i].Image = targetImage
	case types.ImageSwapPolicyExists:
		if p.destinationRegistryClient.ImageExists(lctx, targetRef) {
			log.Ctx(lctx).Debug().Str("image", targetImage).Msg("set new container image")
			containers[i].Image = targetImage
		} else {
			log.Ctx(lctx).Debug().Str("image", targetImage).Msg("container image not found in target registry, not swapping")
		}
	default:
		panic("unknown imageSwapPolicy")
	}
}

// filterMatch returns true if one of the filters matches the context
func filterMatch(ctx FilterContext, filters []config.JMESPathFilter) bool {
	// Simplify FilterContext to be easier searchable by marshaling it to JSON and back to an interface
	var filterContext interface{}
	jsonBlob, err := json.Marshal(ctx)
	if err != nil {
		log.Err(err).Msg("could not marshal filter context")
		return false
	}

	err = json.Unmarshal(jsonBlob, &filterContext)
	if err != nil {
		log.Err(err).Msg("could not unmarshal json blob")
		return false
	}

	log.Debug().Interface("object", filterContext).Msg("generated filter context")

	for idx, filter := range filters {
		results, err := jmespath.Search(filter.JMESPath, filterContext)
		log.Debug().Str("filter", filter.JMESPath).Interface("results", results).Msg("jmespath search results")

		if err != nil {
			log.Err(err).Str("filter", filter.JMESPath).Msgf("Filter (idx %v) could not be evaluated.", idx)
			return false
		}

		switch results.(type) {
		case bool:
			if results == true {
				return true
			}
		default:
			log.Warn().Str("filter", filter.JMESPath).Msg("filter does not return a bool value")
		}
	}

	return false
}

// targetName returns the reference in the target repository
func (p *ImageSwapper) targetRef(targetRef ctypes.ImageReference) ctypes.ImageReference {
	//targetImage := fmt.Sprintf("%s/%s", p.destinationRegistryClient.Endpoint(), targetRef.DockerReference().String())
	targetImage := fmt.Sprintf("%s/%s", p.destinationRegistryClient.Endpoint(), targetRef.DockerReference().String())

	ref, err := alltransports.ParseImageName("docker://" + targetImage)
	if err != nil {
		log.Warn().Msgf("invalid target name %s: %v", targetImage, err)
	}

	return ref
}

// FilterContext is being used by JMESPath to search and match
type FilterContext struct {
	// Obj contains the object submitted to the webhook (currently only pods)
	Obj metav1.Object `json:"obj,omitempty"`

	// Container contains the currently processed container
	Container corev1.Container `json:"container,omitempty"`
}

func NewFilterContext(request kwhmodel.AdmissionReview, obj metav1.Object, container corev1.Container) FilterContext {
	if obj.GetNamespace() == "" {
		obj.SetNamespace(request.Namespace)
	}

	return FilterContext{Obj: obj, Container: container}
}
