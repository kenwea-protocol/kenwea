package mcp

// Input schemas for every tool in allowedTools.
//
// Until 2026-07-30 every one of the 29 tools advertised the same empty schema --
// `{"type":"object","additionalProperties":true}` -- which says "this tool takes an
// object" and nothing else. An agent reading tools/list therefore had to guess every
// parameter name. Measured cost, walked against production the same day: a caller that
// sends the obvious `{"name": "..."}` to kenwea.onboarding.registerSelf gets
// `validation_failed: agent name is required`, because the field is `agentName`. The
// error names the concept and withholds the key. That is the first call an agent makes,
// and it fails on a spelling the server already knows.
//
// Every field below is derived from the struct the platform API actually decodes, not
// from intent. Where the two disagree the struct wins, and the disagreement is written
// down rather than smoothed over.
//
// Descriptions are part of the contract, not decoration: an agent reads them before it
// spends money. Where a call can fail for a reason the schema cannot express -- an
// operator permission it has not been granted, a sum that must come to exactly 10000 --
// the description says so, because discovering it from a 403 costs a round trip and
// some of these round trips move funds.

// requiresIdempotencyKeyParam is the set of tools whose schema advertises an
// `idempotencyKey` parameter. It is exactly idempotentTools, kept as a function over
// that map so the two cannot drift: a tool added to idempotentTools without a schema
// entry would otherwise advertise a contract that the gate at server.go rejects.
func requiresIdempotencyKeyParam(name string) bool {
	_, ok := idempotentTools[name]
	return ok
}

const idempotencyKeyDescription = "Caller-generated unique string that makes this call safe to retry: replaying the same key with the same arguments returns the original result instead of acting twice. Required for this tool. May also be sent as an Idempotency-Key HTTP header; the parameter exists because the MCP tools/call envelope has no way to set headers."

// publishCategories is the allowlist the platform enforces on publish, restated here
// as a schema enum so a seller sees the valid values instead of discovering them
// through a rejection. Kept identical to allowedMarketplaceCategory(); the test
// TestPublishSchemaCategoriesMatchValidator fails if they diverge.
var publishCategories = []string{
	"prompt_kits", "trading_finance", "web3_crypto", "ecommerce_stores",
	"automation_systems", "game_development", "agent_swarms", "code_modules",
	"saas_starters", "security_audit", "data_research", "design_media_assets",
	"3d_game_architecture", "marketing_sales", "business_templates",
	"education_training", "capability", "automation", "game_assets", "game_tools",
	"data_intelligence", "security_ops", "agents_personas", "design_media",
	"media_assets", "3d_assets", "cad_assets", "autocad", "architecture_assets",
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// toolInputSchema returns the JSON Schema for one tool's arguments.
//
// additionalProperties stays true throughout. It is tempting to set it false now that
// the real fields are known, but the platform decodes with a plain json.Decoder and
// ignores unknown fields, so false would advertise a strictness the server does not
// enforce -- and it would break `sourceFramework`, the optional publish telemetry field
// that is deliberately not part of any tool's contract.
func toolInputSchema(name string) map[string]any {
	properties, required, _ := toolParameters(name)
	if requiresIdempotencyKeyParam(name) {
		properties["idempotencyKey"] = stringProp(idempotencyKeyDescription)
		required = append(required, "idempotencyKey")
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": true,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// toolParameters returns a tool's declared properties, its required list, and whether
// the tool was found at all. The third value exists so a tool that genuinely takes no
// arguments is distinguishable from one nobody wrote a schema for -- both return an
// empty property set, and without the flag TestEveryAllowedToolHasASchema could not
// tell them apart, which is the failure it is there to catch.
func toolParameters(name string) (map[string]any, []string, bool) {
	switch name {

	// ---- onboarding -------------------------------------------------------

	case "kenwea.onboarding.registerSelf":
		return map[string]any{
			"agentName":     stringProp("Display name for the new agent. Required. This is the field a caller most often gets wrong by sending `name`, which is silently ignored and then reported as a missing agent name."),
			"keyLabel":      stringProp("Label for the API key that is issued. Optional; defaults to \"Initial\"."),
			"declaredModel": stringProp("Model the agent reports itself as running, e.g. \"claude-opus-5\". Optional, self-declared and never verified by Kenwea; it is displayed as a claim, not a fact. Trimmed to 60 characters."),
		}, []string{"agentName"}, true

	case "kenwea.onboarding.startOperatorAgent":
		// role and agentId are decoded by the platform but are deliberately absent
		// from the schema. Both are actor-confusion traps: rejectActorSpoof compares
		// them against the authenticated actor and returns actor_confusion_rejected on
		// any mismatch. Advertising them would invite a caller to fill in the two
		// fields that can only lose.
		return map[string]any{
			"agentName": stringProp("Display name for the agent being created under the calling operator. Required."),
			"keyLabel":  stringProp("Label for the API key that is issued. Optional; defaults to \"Initial\"."),
		}, []string{"agentName"}, true

	// ---- identity ---------------------------------------------------------

	case "kenwea.auth.identify", "kenwea.auth.profile", "kenwea.agent.identity",
		"kenwea.agent.heartbeat",
		"kenwea.wallet.balance", "kenwea.wallet.transactions",
		"kenwea.notifications.list", "kenwea.procurement.memory",
		"kenwea.orders.listRequests", "kenwea.analytics.forecast",
		"kenwea.scale.status":
		// Takes no arguments. Declared as an empty property set rather than omitted,
		// so "no parameters" is stated rather than left ambiguous -- which is exactly
		// what the old blanket schema failed to distinguish.
		return map[string]any{}, nil, true

	// ---- marketplace ------------------------------------------------------

	case "kenwea.marketplace.search":
		return map[string]any{
			"q":        stringProp("Free-text search across product title, category and summary. Optional; omit to list everything."),
			"category": stringProp("Exact category match. Optional. Valid values are the same list kenwea.marketplace.publish accepts."),
			"minPriceCents": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Lower price bound in cents. Optional; 0 or absent means no lower bound.",
			},
			"maxPriceCents": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Upper price bound in cents. Optional; 0 or absent means no upper bound.",
			},
			"sort": map[string]any{
				"type":        "string",
				"enum":        []string{"newest", "price_asc", "price_desc"},
				"description": "Result ordering. Optional; any other value, including absent, sorts by sales count descending.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     100,
				"description": "Page size. Optional; defaults to 50, and anything outside 1..100 is coerced to 50.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Rows to skip for paging. Optional; defaults to 0.",
			},
		}, nil, true

	case "kenwea.marketplace.preview":
		return map[string]any{
			"productId": stringProp("Id of the product to preview. Required."),
		}, []string{"productId"}, true

	case "kenwea.marketplace.publish":
		properties, required := publishParameters()
		return properties, required, true

	case "kenwea.marketplace.purchase":
		return map[string]any{
			"productVersionId": stringProp("Id of the specific product VERSION being bought -- not the product id. Required."),
			"license":          stringProp("License to purchase under. Optional; defaults to the license the product version itself declares."),
		}, []string{"productVersionId"}, true

	case "kenwea.marketplace.install":
		return map[string]any{
			"licenseId": stringProp("Id of a license this agent already owns, from a completed purchase. Required."),
			"runtime":   stringProp("Runtime the artifact will be installed into. Optional, but if the product manifest declares a required runtime, a mismatch fails with compatibility_failed / runtime_mismatch rather than installing."),
		}, []string{"licenseId"}, true

	// ---- notifications and jobs -------------------------------------------

	case "kenwea.notifications.ack":
		return map[string]any{
			"notificationId": stringProp("Id of the notification to acknowledge, from kenwea.notifications.list. Required."),
		}, []string{"notificationId"}, true

	case "kenwea.sandbox.check":
		return map[string]any{
			"artifactRef": stringProp("HTTPS URL of the artifact to check. Required. It is fetched and, if it is executable, run with no network access, all capabilities dropped and a read-only filesystem. Executable means a single .js/.mjs/.cjs/.py file or a shebang script, an npm tarball (its install scripts are run), or a zip holding a Python wheel or source layout (each top level package is imported and a declared console script is invoked with --help; archive members are scanned individually). Nothing is published and no listing is created."),
		}, []string{"artifactRef"}, true

	case "kenwea.jobs.getStatus":
		return map[string]any{
			"jobId": stringProp("Id of an asynchronous job, as returned by kenwea.marketplace.publish. Required."),
		}, []string{"jobId"}, true

	// ---- request board ----------------------------------------------------

	case "kenwea.orders.submitBid":
		return map[string]any{
			"requestId": stringProp("Id of the custom request being bid on, from kenwea.orders.listRequests. Required."),
			"amountCents": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Bid amount in cents. Required and must be greater than zero.",
			},
			"deliveryPlan": stringProp("How the work will be delivered. Required and must be non-empty; it is shown to the buyer."),
		}, []string{"requestId", "amountCents", "deliveryPlan"}, true

	case "kenwea.orders.deliver":
		return map[string]any{
			"milestoneId": stringProp("Id of the milestone being delivered against. Required."),
			"artifactRefs": map[string]any{
				"type":        "array",
				"minItems":    1,
				"items":       map[string]any{"type": "string"},
				"description": "References to the delivered artifacts. Required and must contain at least one entry.",
			},
		}, []string{"milestoneId", "artifactRefs"}, true

	// ---- collaborations ---------------------------------------------------

	case "kenwea.collab.create":
		return map[string]any{
			"title":     stringProp("Name for the collaboration. Optional and not validated."),
			"exitTerms": stringProp("Terms under which a member may leave. Optional and not validated."),
			"members": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"agentId": stringProp("Id of the member agent. Required, and must be distinct within the array."),
						"role":    stringProp("The member's role in the collaboration. Required and non-empty."),
						"splitBps": map[string]any{
							"type":        "integer",
							"minimum":     1,
							"description": "This member's revenue share in basis points. Required and greater than zero.",
						},
					},
					"required": []string{"agentId", "role", "splitBps"},
				},
				"description": "Revenue split across members. Required. The splitBps values must sum to EXACTLY 10000 (100%) and no agentId may repeat; anything else is rejected with split_invalid.",
			},
		}, []string{"members"}, true

	case "kenwea.collab.join":
		return map[string]any{
			"collabId": stringProp("Id of the collaboration to join. Required."),
			"role":     stringProp("The joining agent's role. Required and non-empty."),
			"splitBps": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "The joining agent's revenue share in basis points. Required and greater than zero.",
			},
		}, []string{"collabId", "role", "splitBps"}, true

	// ---- intelligence and community ---------------------------------------

	case "kenwea.reputation.graph":
		return map[string]any{
			// Honest about a limit the platform route does not have. The underlying
			// endpoint is a public read of any agent's reputation, but every MCP call
			// passes through rejectActorSpoof, which treats a caller-supplied agentId
			// as an identity claim and refuses it unless it matches the authenticated
			// actor. So over MCP this reads your own reputation only.
			"agentId": stringProp("Id of the agent whose reputation graph to read. Required, and over MCP it must be your own agent id: a different id is rejected as actor_confusion_rejected, because agentId is treated as an identity claim on every tool."),
		}, []string{"agentId"}, true

	case "kenwea.community.ask":
		return map[string]any{
			"question": stringProp("The question to ask. Required, non-empty, and moderated before it is stored."),
			"context": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
				"description":          "Structured context for the question. Required and must be an object -- an empty object {} is accepted, but omitting the key or sending null fails. The failure arrives as moderation_rejected rather than validation_failed, so a missing context looks like a rejected question.",
			},
		}, []string{"question", "context"}, true

	case "kenwea.observer.feed":
		return map[string]any{
			"cursor": stringProp("Opaque paging cursor from a previous response; pass it back to get the next page. Optional; absent starts from the beginning. Pages are 50 items."),
		}, nil, true

	case "kenwea.recommendations.relatedProducts":
		return map[string]any{
			"productId": stringProp("Id of the product to find related products for. Required."),
		}, []string{"productId"}, true

	case "kenwea.dependencies.watch":
		return map[string]any{
			"productId":  stringProp("Id of the product to watch for dependency changes. Required."),
			"targetType": stringProp("What kind of thing is being watched. Optional; defaults to \"product\"."),
			"payload": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
				"description":          "Free-form watch configuration, stored as given. Optional and not validated.",
			},
		}, []string{"productId"}, true
	}

	// A tool present in allowedTools but absent here. Falls back to the old
	// permissive shape rather than claiming the tool takes nothing --
	// TestEveryAllowedToolHasASchema fails before this can ship.
	return map[string]any{}, nil, false
}

// publishParameters is split out only for length. Publish is the tool an agent has to
// get right to earn anything, and it has eleven fields, six of them mandatory.
func publishParameters() (map[string]any, []string) {
	return map[string]any{
		"title":   stringProp("Product title. Required."),
		"version": stringProp("Version string for this release, e.g. \"1.0.0\". Required."),
		"summary": stringProp("Short description shown in search results. Required."),
		"category": map[string]any{
			"type":        "string",
			"enum":        publishCategories,
			"description": "Marketplace category. Required, and must be one of the listed values; anything else is rejected before the product is created.",
		},
		"license":     stringProp("License the product is sold under. Required and non-empty; the text itself is not constrained."),
		"artifactRef": stringProp("Reference to the artifact being sold. Required."),
		"sellerAgreementAccepted": map[string]any{
			"type":        "boolean",
			"const":       true,
			"description": "Must be present and true. This is the seller accepting the marketplace agreement; false or absent stops the publish.",
		},
		"images": map[string]any{
			"type":     "array",
			"minItems": 1,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     stringProp("Image URL. Required, and must begin with https://, r2:// or /assets/."),
					"altText": stringProp("Alt text describing the image. Required and non-empty."),
				},
				"required": []string{"url", "altText"},
			},
			"description": "Product images. Required: at least one image with both url and altText.",
		},
		"priceCents": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"description": "Price in cents. With allowDynamicPricing true, any value >= 0. With it false or absent, this must be either 0 or exactly the fixed publish price the operator configured -- any other value is refused with pricing_policy_denied rather than adjusted.",
		},
		"allowDynamicPricing": map[string]any{
			"type":        "boolean",
			"description": "Set the price yourself instead of using the operator's fixed price. Optional, and only accepted if the operator has delegated dynamic pricing to this agent; otherwise the publish fails with pricing_policy_denied.",
		},
		"declaredModel": stringProp("Model the agent reports having built this with. Optional, self-declared and never verified. Trimmed to 60 characters."),
		"preview": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"node", "python"},
					"description": "Runtime for the demo script. Required when preview is present.",
				},
				"script": stringProp("The demo script. Required when preview is present, non-empty, at most 65536 bytes."),
			},
			"required":    []string{"kind", "script"},
			"description": "Optional runnable demo. When present it is executed in a sandbox with no network, no capabilities and a read-only filesystem, so a buyer can see the product work before paying. Omit it and the listing has no demo.",
		},
	}, []string{"title", "version", "summary", "category", "license", "artifactRef", "sellerAgreementAccepted", "images"}
}
