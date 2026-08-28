package catalog

import (
	"fmt"
	"slices"
	"strings"
)

// The vendor registry: which API platforms this gateway knows how to reach, and
// what a working provider record for each of them looks like.
//
// A vendor answers "whose platform is this", which is a different question from
// the protocol a provider speaks. One vendor publishes several protocols
// (DeepSeek serves both the OpenAI and the Anthropic dialects), and one protocol
// is spoken by dozens of vendors. Conflating the two is what let a organization's
// OpenAI credential be sent to a different company's endpoint, and what made
// "OpenAI" the label shown for a DeepSeek upstream. See ADR-0140.
//
// *This registry is a prefill source, not a runtime dependency.* Creating a
// provider copies the base URL, the protocols and the transport profile out of
// it; from that moment the stored record is the only thing the data plane reads.
// Editing an entry here therefore changes what the next provider is created
// with and nothing about the ones that exist -- the same discipline the bundled
// reference prices follow (ADR-0128): whoever pressed save owns the value.
//
// At runtime a vendor is only an identity: it decides which organization-supplied
// credential may be used for a candidate, which reference-price scope a route
// resolves against, whether the model-discovery button has anything to call,
// and what the operator sees. *No field here may change a request body.*
// Vendor-specific addressing goes through the transport profile, whose boundary
// (ADR-0127, ADR-0131) is unchanged: address, credential presentation and the
// two hosted envelopes, never a judgement about the caller's request.
//
// Adding a vendor is adding an entry below. It deliberately is not a database
// enumeration: the list is knowledge that ships with the code, and a new entry
// should not need a schema change to become selectable.

// VendorKind groups the registry for presentation, and says something real
// about what to expect: a first-party vendor serves its own models, a platform
// hosts other companies' models under its own addressing and credentials, and
// an aggregator resells many vendors behind one endpoint and reports its own
// accounting rather than theirs.
type VendorKind string

const (
	VendorFirstParty VendorKind = "first_party"
	VendorPlatform   VendorKind = "platform"
	VendorAggregator VendorKind = "aggregator"
	VendorKindCustom VendorKind = "custom"
)

// VendorCustom is the escape hatch: any protocol, nothing prefilled. It is what
// every OpenAI- or Anthropic-compatible upstream without an entry of its own
// uses, and it is the reason this registry does not have to be exhaustive to be
// useful.
const VendorCustom = "custom"

// How the credential for a vendor is shaped. The gateway stores whatever it is
// given; this only tells the operator what to paste, which is the difference
// between a working setup and a 401 nobody can explain.
const (
	KeyHintBearer            = "bearer"
	KeyHintAWSKeypairJSON    = "aws_keypair_json"
	KeyHintGCPServiceAccount = "gcp_service_account_json"
	KeyHintKlingKeypairJSON  = "kling_keypair_json"
)

// Metering fidelity, per protocol. It is a *declaration about the endpoint*,
// not a measurement: what an upstream reports can change in a release nobody
// announces, so this is a starting point for choosing between two ways of
// reaching the same model, and the way to be sure remains sending one request
// and reading the usage row.
const (
	FidelityFull       = "full"        // native dialect: cache reads, cache writes and reasoning tokens are all reported
	FidelityPartial    = "partial"     // totals are right, the cache split is missing, so cache reads bill at full input price
	FidelityTotalsOnly = "totals_only" // input and output totals only
	FidelityUnknown    = "unknown"     // not established; send one request and look at the usage row
)

// VendorBaseURL is one endpoint a vendor publishes. Several entries mean a
// genuine choice the operator has to make -- a mainland-China host and an
// international one are different endpoints with different credentials, not a
// mirror pair.
type VendorBaseURL struct {
	// Label distinguishes the options; empty when a vendor has exactly one.
	Label string
	URL   string
	// Template marks a URL carrying a placeholder (a resource name, a region)
	// that has no default and must be replaced before the provider can work.
	// The write path refuses a base URL that still contains one, because a
	// placeholder that reaches the data plane fails as a DNS error, which reads
	// like an outage rather than like an unfinished form.
	Template bool
}

// Vendor is one entry in the registry.
type Vendor struct {
	Slug  string
	Label string
	Kind  VendorKind
	// Protocols is every dialect this vendor publishes; DefaultProtocols is the
	// subset the preset below is wired for -- the protocols its base URL and
	// transport profile actually serve. A provider created without naming its
	// protocols gets this set: for a known vendor, picking it has already said
	// which protocols it speaks. Selecting a protocol outside the default set
	// is allowed and means editing the base URL or the transport profile --
	// Google's OpenAI-compatible surface, for instance, lives at a different
	// path from its native one.
	Protocols        []string
	DefaultProtocols []string
	BaseURLs         []VendorBaseURL
	// Transport is the profile a provider for this vendor starts with. The zero
	// value means "nothing to configure", which is true of most upstreams.
	Transport Transport
	KeyHint   string
	// ModelListing reports whether this vendor publishes a catalogue endpoint
	// the discovery button can call. False is worth stating: without it an
	// upstream that simply has no such endpoint answers the discovery request
	// in a way that renders as "this provider serves no models" -- a conclusion
	// rather than an error, so nothing sends anyone looking.
	ModelListing bool
	// Fidelity is per protocol; a protocol absent from the map is unrated.
	Fidelity map[string]string
	// RefdataProvider is this vendor's id in the bundled reference-price
	// dataset, empty when it has none or when the dataset splits the vendor
	// across ids that only the base URL can tell apart.
	RefdataProvider string
	// Creator is the model-creator segment for the models this platform
	// publishes as a first party -- the first segment of their catalog slug,
	// as in the "openai" of "openai/gpt-5.6-sol".
	//
	// Empty for platforms and aggregators, and that emptiness is the point:
	// they carry other people's models, so they cannot answer "who made this",
	// and a prefix guessed from the platform's own name would write the
	// routing dimension into the catalog key. A caller that cannot get a
	// creator here has to ask a person rather than fall back to something.
	//
	// Three fields on this struct now look alike and answer different
	// questions. They never stand in for one another:
	//
	//   Creator         who made the model      -- the slug's first segment
	//   RefdataProvider who prices it           -- a dataset scope
	//   ModelIDExample  how the upstream spells it -- placeholder text
	//
	// xAI is the illustration: its creator segment and its dataset id are both
	// "x-ai" while its own slug is "xai", so any two of the three agreeing is
	// a coincidence rather than a rule.
	Creator        string
	DocsURL        string
	ModelIDExample string
}

// Vendors returns the registry.
//
// It is built on each call rather than kept in a package variable: the entries
// carry maps, and a shared copy would let one caller's edit reach every other
// caller. The list is short and read at configuration time, never per request.
func Vendors() []Vendor {
	return []Vendor{
		{
			Slug: "openai", Label: "OpenAI", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:        []VendorBaseURL{{URL: "https://api.openai.com"}},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolOpenAI: FidelityFull},
			RefdataProvider: "openai",
			Creator:         "openai",
			DocsURL:         "https://platform.openai.com/docs/api-reference",
			ModelIDExample:  "gpt-4o-mini",
		},
		{
			Slug: "anthropic", Label: "Anthropic", Kind: VendorFirstParty,
			Protocols: []string{ProtocolAnthropic}, DefaultProtocols: []string{ProtocolAnthropic},
			BaseURLs:        []VendorBaseURL{{URL: "https://api.anthropic.com"}},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolAnthropic: FidelityFull},
			RefdataProvider: "anthropic",
			Creator:         "anthropic",
			DocsURL:         "https://docs.anthropic.com/en/api/messages",
			ModelIDExample:  "claude-sonnet-4-20250514",
		},
		{
			// Two ways in, and the native one is the default on purpose: the
			// OpenAI-compatible surface does not report context caching, so a
			// cache read there is billed to the caller at the full input rate.
			//
			// The preset is for the native protocol, whose paths this gateway
			// sends as they are. Reaching the compatible surface instead means
			// pointing the base URL at /v1beta/openai and stripping the version
			// segment from each path -- that endpoint carries its own -- which is
			// why this entry does not try to prefill both at once. The recipe is
			// in the documentation.
			Slug: "google", Label: "Google Gemini", Kind: VendorFirstParty,
			// Also the video job plane: Veo is served from the same host on
			// its own long-running-operation API.
			Protocols:        []string{ProtocolGemini, ProtocolOpenAI, ProtocolVideo},
			DefaultProtocols: []string{ProtocolGemini},
			BaseURLs:         []VendorBaseURL{{URL: "https://generativelanguage.googleapis.com"}},
			KeyHint:          KeyHintBearer,
			ModelListing:     true,
			Fidelity: map[string]string{
				ProtocolGemini: FidelityFull,
				ProtocolOpenAI: FidelityPartial,
			},
			RefdataProvider: "google",
			Creator:         "google",
			DocsURL:         "https://ai.google.dev/gemini-api/docs",
			ModelIDExample:  "gemini-2.5-flash",
		},
		{
			// Claude on Vertex: the project and the region are written into the
			// paths because that is where Vertex puts them, and the region also
			// appears in the hostname, where the two have to agree.
			//
			// Anthropic only, deliberately. Vertex also serves Gemini, at a
			// different address and with no envelope -- and an envelope applies
			// to whatever this record sends, so one record cannot carry both.
			// That combination is a second provider record; the recipe is in the
			// documentation.
			Slug: "google-vertex", Label: "Google Vertex AI", Kind: VendorPlatform,
			Protocols: []string{ProtocolAnthropic}, DefaultProtocols: []string{ProtocolAnthropic},
			BaseURLs: []VendorBaseURL{{URL: "https://{region}-aiplatform.googleapis.com", Template: true}},
			Transport: Transport{
				Auth:     AuthGCPServiceAccount,
				Envelope: EnvelopeVertex,
				PathOverrides: map[string]string{
					PathMessages: "/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:rawPredict",
				},
				StreamPathOverrides: map[string]string{
					PathMessages: "/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:streamRawPredict",
				},
			},
			KeyHint:         KeyHintGCPServiceAccount,
			ModelListing:    false,
			Fidelity:        map[string]string{ProtocolAnthropic: FidelityFull},
			RefdataProvider: "google",
			DocsURL:         "https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/use-claude",
			ModelIDExample:  "claude-sonnet-4@20250514",
		},
		{
			// The v1 surface, which takes OpenAI-shaped paths directly; only the
			// credential presentation differs. The upstream model name on the
			// route is the deployment name.
			Slug: "azure-openai", Label: "Azure OpenAI", Kind: VendorPlatform,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:        []VendorBaseURL{{URL: "https://{resource}.openai.azure.com/openai", Template: true}},
			Transport:       Transport{Auth: AuthHeaderPrefix + "api-key"},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolOpenAI: FidelityFull},
			RefdataProvider: "azure",
			DocsURL:         "https://learn.microsoft.com/azure/ai-services/openai/reference",
			ModelIDExample:  "my-gpt-4o-deployment",
		},
		{
			// Bedrock's native Anthropic endpoint: signed requests, the model in
			// the path, and streaming at a second address. Its OpenAI-compatible
			// endpoint is a different base URL with no envelope, so it is a
			// separate provider record rather than a second protocol on this one.
			Slug: "aws-bedrock", Label: "AWS Bedrock", Kind: VendorPlatform,
			Protocols: []string{ProtocolAnthropic}, DefaultProtocols: []string{ProtocolAnthropic},
			BaseURLs: []VendorBaseURL{{URL: "https://bedrock-runtime.{region}.amazonaws.com", Template: true}},
			Transport: Transport{
				Auth:                AuthAWSSigV4,
				Envelope:            EnvelopeBedrock,
				SigV4:               &SigV4Profile{Region: "us-east-1"},
				PathOverrides:       map[string]string{PathMessages: "/model/{model}/invoke"},
				StreamPathOverrides: map[string]string{PathMessages: "/model/{model}/invoke-with-response-stream"},
			},
			KeyHint:         KeyHintAWSKeypairJSON,
			ModelListing:    false,
			Fidelity:        map[string]string{ProtocolAnthropic: FidelityFull},
			RefdataProvider: "aws",
			DocsURL:         "https://docs.aws.amazon.com/bedrock/latest/userguide/",
			ModelIDExample:  "anthropic.claude-sonnet-4-20250514-v1:0",
		},
		{
			Slug: "xai", Label: "xAI", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:        []VendorBaseURL{{URL: "https://api.x.ai"}},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolOpenAI: FidelityUnknown},
			RefdataProvider: "x-ai",
			Creator:         "x-ai",
			DocsURL:         "https://docs.x.ai/docs/api-reference",
			ModelIDExample:  "grok-4",
		},
		{
			Slug: "mistral", Label: "Mistral AI", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:        []VendorBaseURL{{URL: "https://api.mistral.ai"}},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolOpenAI: FidelityUnknown},
			RefdataProvider: "mistral",
			Creator:         "mistralai",
			DocsURL:         "https://docs.mistral.ai/api/",
			ModelIDExample:  "mistral-large-latest",
		},
		{
			// A relay in front of many vendors. It reports its own accounting,
			// not the underlying vendor's, and the two do not always agree.
			Slug: "openrouter", Label: "OpenRouter", Kind: VendorAggregator,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:        []VendorBaseURL{{URL: "https://openrouter.ai/api"}},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolOpenAI: FidelityTotalsOnly},
			RefdataProvider: "openrouter",
			DocsURL:         "https://openrouter.ai/docs",
			ModelIDExample:  "anthropic/claude-sonnet-4",
		},
		{
			Slug: "groq", Label: "Groq", Kind: VendorAggregator,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:        []VendorBaseURL{{URL: "https://api.groq.com/openai"}},
			KeyHint:         KeyHintBearer,
			ModelListing:    true,
			Fidelity:        map[string]string{ProtocolOpenAI: FidelityTotalsOnly},
			RefdataProvider: "groq",
			DocsURL:         "https://console.groq.com/docs/api-reference",
			ModelIDExample:  "llama-3.3-70b-versatile",
		},
		{
			// Both dialects behind one host: the Anthropic one is the same base
			// with an /anthropic prefix, which the override supplies.
			Slug: "deepseek", Label: "DeepSeek", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI, ProtocolAnthropic}, DefaultProtocols: []string{ProtocolOpenAI, ProtocolAnthropic},
			BaseURLs:     []VendorBaseURL{{URL: "https://api.deepseek.com"}},
			Transport:    Transport{PathOverrides: map[string]string{PathMessages: "/anthropic/v1/messages"}},
			KeyHint:      KeyHintBearer,
			ModelListing: true,
			Fidelity: map[string]string{
				ProtocolOpenAI:    FidelityPartial,
				ProtocolAnthropic: FidelityUnknown,
			},
			RefdataProvider: "deepseek",
			Creator:         "deepseek",
			DocsURL:         "https://api-docs.deepseek.com/",
			ModelIDExample:  "deepseek-chat",
		},
		{
			// Two families behind two addresses. The text models speak
			// OpenAI's dialect under /compatible-mode; Wan's video task API is
			// native and sits at the host root. One record cannot carry both --
			// the base URL is a property of the record -- so a Wan channel is a
			// second provider record, the same shape Vertex and Bedrock need.
			// The recipe is in the documentation.
			Slug: "alibaba", Label: "阿里云百炼 (Alibaba Model Studio)", Kind: VendorFirstParty,
			Protocols:        []string{ProtocolOpenAI, ProtocolVideo},
			DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs: []VendorBaseURL{
				{Label: "China", URL: "https://dashscope.aliyuncs.com/compatible-mode"},
				{Label: "International", URL: "https://dashscope-intl.aliyuncs.com/compatible-mode"},
				{Label: "China (video)", URL: "https://dashscope.aliyuncs.com"},
				{Label: "International (video)", URL: "https://dashscope-intl.aliyuncs.com"},
			},
			KeyHint:        KeyHintBearer,
			ModelListing:   true,
			Fidelity:       map[string]string{ProtocolOpenAI: FidelityTotalsOnly},
			Creator:        "qwen",
			DocsURL:        "https://help.aliyun.com/zh/model-studio/compatibility-of-openai-with-dashscope",
			ModelIDExample: "qwen-plus",
		},
		{
			Slug: "moonshot", Label: "Moonshot AI (Kimi)", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI, ProtocolAnthropic}, DefaultProtocols: []string{ProtocolOpenAI, ProtocolAnthropic},
			BaseURLs: []VendorBaseURL{
				{Label: "China", URL: "https://api.moonshot.cn"},
				{Label: "International", URL: "https://api.moonshot.ai"},
			},
			Transport:    Transport{PathOverrides: map[string]string{PathMessages: "/anthropic/v1/messages"}},
			KeyHint:      KeyHintBearer,
			ModelListing: true,
			Fidelity: map[string]string{
				ProtocolOpenAI:    FidelityTotalsOnly,
				ProtocolAnthropic: FidelityUnknown,
			},
			RefdataProvider: "moonshotai",
			Creator:         "moonshotai",
			DocsURL:         "https://platform.moonshot.cn/docs/api-reference",
			ModelIDExample:  "kimi-k2-0905-preview",
		},
		{
			// The base URL stops at the host so that one record can reach both
			// dialects: the OpenAI one lives under /api/paas/v4 and the
			// Anthropic one under /api/anthropic, and neither carries the
			// version segment this gateway would send on its own.
			//
			// RefdataProvider is deliberately empty: the dataset splits this
			// vendor into two ids (zhipuai and zai) that only the base URL can
			// tell apart, so the base-URL pass is the one that answers correctly
			// for whichever host was chosen.
			Slug: "zhipu", Label: "智谱 AI (Zhipu / Z.ai)", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI, ProtocolAnthropic}, DefaultProtocols: []string{ProtocolOpenAI, ProtocolAnthropic},
			BaseURLs: []VendorBaseURL{
				{Label: "China", URL: "https://open.bigmodel.cn"},
				{Label: "International", URL: "https://api.z.ai"},
			},
			Transport: Transport{PathOverrides: map[string]string{
				PathChat:       "/api/paas/v4/chat/completions",
				PathEmbeddings: "/api/paas/v4/embeddings",
				PathModels:     "/api/paas/v4/models",
				PathMessages:   "/api/anthropic/v1/messages",
			}},
			KeyHint:      KeyHintBearer,
			ModelListing: true,
			Fidelity: map[string]string{
				ProtocolOpenAI:    FidelityTotalsOnly,
				ProtocolAnthropic: FidelityUnknown,
			},
			Creator:        "z-ai",
			DocsURL:        "https://docs.bigmodel.cn/api-reference",
			ModelIDExample: "glm-4.6",
		},
		{
			// Ark puts its own version segment in the base URL, so the paths
			// this gateway sends lose theirs. The upstream model name is either
			// the model id or an endpoint id, depending on how the account is
			// set up.
			Slug: "volcengine", Label: "火山方舟 (Volcengine Ark)", Kind: VendorFirstParty,
			// Also the video job plane: Seedance is served from the same
			// account and base URL, on this vendor's own task API.
			Protocols: []string{ProtocolOpenAI, ProtocolVideo}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs: []VendorBaseURL{
				{Label: "China", URL: "https://ark.cn-beijing.volces.com/api/v3"},
				{Label: "International", URL: "https://ark.ap-southeast.bytepluses.com/api/v3"},
			},
			Transport: Transport{PathOverrides: map[string]string{
				PathChat:       "/chat/completions",
				PathEmbeddings: "/embeddings",
				PathModels:     "/models",
				// Seedream is served from this same base URL in OpenAI's own
				// image shape. Without this override the request is addressed
				// to <base>/v1/images/generations -- the base URL already ends
				// in /api/v3 -- and the upstream answers 404, which reads as
				// "the model is not there" rather than "the path is wrong".
				//
				// The edits path is deliberately not overridden: this vendor
				// has no separate edits endpoint, it takes an input image on
				// the generations call. Leaving it out lets the probe record
				// `unsupported` for edits, which is the truth.
				//
				// That sentence was false when it was written, and it is worth
				// saying why rather than quietly fixing it: generations and
				// edits shared one capability key, and only generations was
				// ever probed. So nothing recorded anything about edits, and a
				// route verified here became a candidate for an endpoint that
				// answers 404 on every request. `images_edits` is a surface of
				// its own now, and the sentence is true.
				PathImagesGenerate: "/images/generations",
			}},
			KeyHint:        KeyHintBearer,
			ModelListing:   true,
			Fidelity:       map[string]string{ProtocolOpenAI: FidelityUnknown},
			Creator:        "bytedance",
			DocsURL:        "https://www.volcengine.com/docs/82379",
			ModelIDExample: "doubao-seed-1-6",
		},
		{
			Slug: "baidu", Label: "百度千帆 (Baidu Qianfan)", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs: []VendorBaseURL{{URL: "https://qianfan.baidubce.com/v2"}},
			Transport: Transport{PathOverrides: map[string]string{
				PathChat:       "/chat/completions",
				PathEmbeddings: "/embeddings",
				PathModels:     "/models",
			}},
			KeyHint:        KeyHintBearer,
			ModelListing:   true,
			Fidelity:       map[string]string{ProtocolOpenAI: FidelityUnknown},
			Creator:        "baidu",
			DocsURL:        "https://cloud.baidu.com/doc/qianfan-api/index.html",
			ModelIDExample: "ernie-4.5-turbo-128k",
		},
		{
			Slug: "tencent", Label: "腾讯混元 (Tencent Hunyuan)", Kind: VendorFirstParty,
			Protocols: []string{ProtocolOpenAI}, DefaultProtocols: []string{ProtocolOpenAI},
			BaseURLs:       []VendorBaseURL{{URL: "https://api.hunyuan.cloud.tencent.com"}},
			KeyHint:        KeyHintBearer,
			ModelListing:   true,
			Fidelity:       map[string]string{ProtocolOpenAI: FidelityUnknown},
			Creator:        "tencent",
			DocsURL:        "https://cloud.tencent.com/document/product/1729/111007",
			ModelIDExample: "hunyuan-turbos-latest",
		},
		{
			// The first vendor in the registry that speaks *only* the video
			// plane. It is a useful shape to have: it proves the protocol axis
			// really is orthogonal rather than an add-on to a text vendor.
			Slug: "kuaishou", Label: "可灵 (Kling)", Kind: VendorFirstParty,
			Protocols:        []string{ProtocolVideo},
			DefaultProtocols: []string{ProtocolVideo},
			BaseURLs:         []VendorBaseURL{{URL: "https://api-beijing.klingai.com"}},
			// This platform authenticates with a JWT the caller signs, valid for
			// half an hour. Pasting one in as a static key works until it
			// expires and then answers 401 on every request, so what is stored
			// is the key pair and the token is minted per request.
			Transport:      Transport{Auth: AuthKlingJWT},
			KeyHint:        KeyHintKlingKeypairJSON,
			ModelListing:   false,
			Creator:        "kuaishou",
			DocsURL:        "https://app.klingai.com/global/dev/document-api",
			ModelIDExample: "kling-v2-master",
		},
		{
			// Also the video job plane: Hailuo is served from the same host and
			// the same account as the text models, so one record carries both --
			// unlike Wan, whose video API is at a different address from its
			// text one.
			Slug: "minimax", Label: "MiniMax", Kind: VendorFirstParty,
			Protocols:        []string{ProtocolOpenAI, ProtocolAnthropic, ProtocolVideo},
			DefaultProtocols: []string{ProtocolOpenAI, ProtocolAnthropic},
			BaseURLs: []VendorBaseURL{
				{Label: "China", URL: "https://api.minimaxi.com"},
				{Label: "International", URL: "https://api.minimax.io"},
			},
			Transport:    Transport{PathOverrides: map[string]string{PathMessages: "/anthropic/v1/messages"}},
			KeyHint:      KeyHintBearer,
			ModelListing: true,
			Fidelity: map[string]string{
				ProtocolOpenAI:    FidelityUnknown,
				ProtocolAnthropic: FidelityUnknown,
			},
			RefdataProvider: "minimax",
			Creator:         "minimax",
			DocsURL:         "https://platform.minimax.io/docs/api-reference",
			ModelIDExample:  "MiniMax-M2",
		},
		{
			// Last on purpose: it is the answer when none of the above is, and a
			// list of known platforms ending in "something else" reads the way
			// the choice actually works.
			Slug: VendorCustom, Label: "Custom (OpenAI- or Anthropic-compatible)", Kind: VendorKindCustom,
			Protocols:        WireProtocols(),
			DefaultProtocols: []string{ProtocolOpenAI},
			KeyHint:          KeyHintBearer,
			ModelListing:     true,
			DocsURL:          "",
			ModelIDExample:   "",
		},
	}
}

// LookupVendor finds one entry by slug.
func LookupVendor(slug string) (Vendor, bool) {
	for _, v := range Vendors() {
		if v.Slug == slug {
			return v, true
		}
	}
	return Vendor{}, false
}

// VendorLabel is the display name for a slug, falling back to the slug itself.
func VendorLabel(slug string) string {
	if v, ok := LookupVendor(slug); ok {
		return v.Label
	}
	return slug
}

// CheckProtocols reports whether a set of protocols may be declared for a
// provider of this vendor.
//
// The rule is per vendor rather than global because "this platform does not
// speak that dialect" is a mistake worth catching while somebody is looking at
// the form: declared but unspoken, it produces routes that save fine, never
// serve, and show as green.
//
// The custom vendor accepts every known protocol, which is the whole point of
// it -- an upstream nobody has written an entry for is exactly the case where
// this registry has no opinion.
func (v Vendor) CheckProtocols(protocols []string) error {
	if len(protocols) == 0 {
		return fmt.Errorf("at least one protocol is required")
	}
	for _, p := range protocols {
		if _, ok := ProtocolEndpoints(p); !ok {
			return fmt.Errorf("unknown protocol %q", p)
		}
		if !slices.Contains(v.Protocols, p) {
			return fmt.Errorf(
				"%s does not publish the %s protocol; it speaks %s",
				v.Label, p, strings.Join(v.Protocols, ", "))
		}
	}
	return nil
}
