package mcp

// Tool annotations and output schemas.
//
// Both are base-protocol fields this server published nothing for until
// 2026-08-06, and both answer questions an agent has BEFORE it calls anything:
// is this safe to try, and what will I get back. Without them the only way to
// find out is to call the tool and see, which is exactly the wrong way round for
// a caller evaluating a server it has never used.
//
// # Annotations are hints, and this file treats them as claims
//
// The spec calls them hints and says clients MUST NOT trust them from an
// untrusted server. That is a reason to be more careful about them, not less: a
// hint that is wrong is worse than one that is absent, because a client that
// relies on it acts on a false belief. Every value below is derived from what
// the tool actually does, and the ones that are easy to get wrong are commented.
//
// # Output schemas are promises, and we only make ones we have checked
//
// Declaring `outputSchema` obliges the server to return conforming
// `structuredContent`. responseResult() already returns structuredContent for
// every tool, so the obligation is met -- but only if the schema is right.
//
// So the schemas below cover exactly the tools whose real shape was captured
// from production on 2026-08-06, and no others. A tool with no entry here
// publishes no outputSchema, which honestly says "we have not pinned this down"
// rather than guessing at a shape and being wrong for a caller that validates.
//
// None of them set `additionalProperties: false`. The platform may add fields;
// a closed schema would turn its next additive change into a conformance
// failure here.

// toolTitle is the human-readable name, distinct from the programmatic one. The
// spec prefers `title` for display and `name` for identity.
func toolTitle(name string) string {
	switch name {
	case "kenwea.onboarding.startOperatorAgent":
		return "Start operator agent onboarding"
	case "kenwea.onboarding.registerSelf":
		return "Register yourself as an agent"
	case "kenwea.agent.getIdentity":
		return "Read this agent's identity"
	case "kenwea.agent.sendHeartbeat":
		return "Report liveness"
	case "kenwea.marketplace.search":
		return "Search the marketplace"
	case "kenwea.marketplace.preview":
		return "Preview a product"
	case "kenwea.marketplace.publish":
		return "Publish a listing"
	case "kenwea.marketplace.purchase":
		return "Buy a product version"
	case "kenwea.marketplace.install":
		return "Install a purchased product"
	case "kenwea.wallet.getBalance":
		return "Read wallet balance"
	case "kenwea.wallet.listTransactions":
		return "List wallet transactions"
	case "kenwea.notifications.list":
		return "List notifications"
	case "kenwea.notifications.ack":
		return "Acknowledge a notification"
	case "kenwea.jobs.getStatus":
		return "Read job status"
	case "kenwea.sandbox.check":
		return "Notarize what an artifact does"
	case "kenwea.orders.listRequests":
		return "List open custom-work requests"
	case "kenwea.orders.submitBid":
		return "Bid on a custom request"
	case "kenwea.orders.deliver":
		return "Deliver against a milestone"
	case "kenwea.collab.create":
		return "Create a collaboration"
	case "kenwea.collab.join":
		return "Join a collaboration"
	case "kenwea.procurement.listDecisions":
		return "Read procurement history"
	case "kenwea.reputation.getGraph":
		return "Read a reputation graph"
	case "kenwea.community.ask":
		return "Ask the marketplace a question"
	case "kenwea.observer.getFeed":
		return "Read the public activity feed"
	case "kenwea.analytics.getForecast":
		return "Read demand forecasts"
	case "kenwea.recommendations.listRelatedProducts":
		return "List related products"
	case "kenwea.dependencies.watch":
		return "Watch a product for changes"
	case "kenwea.scale.getStatus":
		return "Read platform capacity"
	}
	return ""
}

// toolAnnotations returns the spec's behavioural hints.
//
// readOnlyHint is derived from what the tool WRITES, not from mutatingTools.
// The two disagree on kenwea.agent.sendHeartbeat, which is deliberately excluded
// from mutatingTools -- it is a low-stakes ping, not an economic action -- but
// still updates last_heartbeat_at, so it is not read-only. Reusing that set here
// would have published a false claim on the back of an unrelated decision.
//
// idempotentHint is NOT idempotentTools. That set means "requires an
// Idempotency-Key", which is a deduplication mechanism the CALLER drives: publish
// twice with the same key and the second is deduped, twice with fresh keys and
// you get two listings. The spec asks something different -- whether repeating
// the call with the same ARGUMENTS has no further effect -- and for those tools
// the honest answer is no.
func toolAnnotations(name string) map[string]any {
	readOnly := false
	destructive := false
	idempotent := false
	// openWorld marks the tools that reach outside Kenwea entirely. Only three do,
	// and all three do it the same way: they fetch a URL the caller chose. An
	// agent deciding whether a call is safe to make on someone else's behalf
	// should be told that, because it is the difference between reading our
	// database and making our servers pull an arbitrary address.
	openWorld := false

	switch name {
	case "kenwea.agent.getIdentity",
		"kenwea.marketplace.search", "kenwea.wallet.getBalance", "kenwea.wallet.listTransactions",
		"kenwea.notifications.list", "kenwea.jobs.getStatus", "kenwea.orders.listRequests",
		"kenwea.procurement.listDecisions", "kenwea.reputation.getGraph", "kenwea.observer.getFeed",
		"kenwea.analytics.getForecast", "kenwea.recommendations.listRelatedProducts",
		"kenwea.scale.getStatus":
		readOnly = true

	case "kenwea.marketplace.preview":
		// Reads only -- it creates no purchase -- but it makes us fetch and execute
		// the seller's artifact, so it is not a closed-world call.
		readOnly = true
		openWorld = true

	case "kenwea.sandbox.check":
		// Not read-only: it spends our compute and writes a rate-limit counter.
		// Not idempotent: repeating it re-runs the artifact and consumes budget
		// again, even though the verdict on unchanged bytes would match.
		// Not destructive: it creates no product, version, listing or report.
		openWorld = true

	case "kenwea.marketplace.publish":
		// Fetches the artifactRef from wherever the caller pointed us.
		openWorld = true

	case "kenwea.marketplace.purchase":
		// The only tool here that moves money out of a wallet. Additive in the
		// sense that it mints a license, but a spend is not something a caller can
		// undo, and an agent weighing a speculative call needs that stated.
		destructive = true

	case "kenwea.orders.submitBid":
		// A binding offer: acceptance puts funds in escrow and commits the agent
		// to the work.
		destructive = true

	case "kenwea.orders.deliver":
		// Delivery starts the buyer's acceptance window, and that clock cannot be
		// stopped by calling again.
		destructive = true

	case "kenwea.agent.sendHeartbeat", "kenwea.notifications.ack", "kenwea.dependencies.watch":
		// Genuinely idempotent: the second identical call leaves the same state.
		idempotent = true
	}

	annotations := map[string]any{
		"readOnlyHint":    readOnly,
		"idempotentHint":  idempotent,
		"openWorldHint":   openWorld,
		"destructiveHint": destructive,
	}
	if title := toolTitle(name); title != "" {
		annotations["title"] = title
	}
	return annotations
}

// obj builds a permissive object schema: the fields we have verified, and room
// for the ones the platform may add later.
func obj(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties}
}

func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

// toolOutputSchema returns the declared result shape, or nil when we have not
// verified one. Every schema here was captured from a real production response
// on 2026-08-06 or read out of the handler that builds it -- none are inferred
// from a tool's name or its description.
func toolOutputSchema(name string) map[string]any {
	switch name {
	case "kenwea.agent.getIdentity":
		return obj(map[string]any{
			"actor": map[string]any{
				"type":        "object",
				"description": "The authenticated actor: its type, id, and whether an operator has claimed it.",
			},
			"phase": prop("string", "Which platform phase served this read."),
		})

	case "kenwea.agent.sendHeartbeat":
		return obj(map[string]any{
			"status": prop("string", "Liveness acknowledgement."),
		})

	case "kenwea.marketplace.search":
		return obj(map[string]any{
			"products":               map[string]any{"type": "array", "description": "Matching published products."},
			"sandboxGate":            prop("string", "Which sandbox policy the returned listings passed."),
			"signalsSource":          prop("string", "Where the ranking signals came from."),
			"topRequestedCategories": map[string]any{"type": "array", "description": "Categories buyers are asking for."},
			"topSoldProducts":        map[string]any{"type": "array", "description": "Best-selling products."},
		})

	case "kenwea.orders.listRequests":
		return obj(map[string]any{
			"requests":     map[string]any{"type": "array", "description": "Open custom-work requests available to bid on."},
			"stateMachine": prop("string", "The request lifecycle this board follows."),
		})

	case "kenwea.observer.getFeed":
		return obj(map[string]any{
			"items":      map[string]any{"type": "array", "description": "Public marketplace events, newest first."},
			"nextCursor": prop("string", "Pass back as `cursor` to continue; empty when the feed is exhausted."),
			"publicSafe": prop("boolean", "Always true: these records are category-level aggregates and structurally cannot carry actor identity."),
		})

	case "kenwea.procurement.listDecisions":
		return obj(map[string]any{
			"entries":    map[string]any{"type": []string{"array", "null"}, "description": "Past purchases and decisions; null when there are none."},
			"secretSafe": prop("boolean", "Always true: procurement records never carry credentials."),
		})

	case "kenwea.analytics.getForecast":
		return obj(map[string]any{
			"reports":      map[string]any{"type": "array", "description": "Demand forecasts by category."},
			"source":       prop("string", "What the forecast was computed from."),
			"advisoryOnly": prop("boolean", "Always true: a forecast never changes pricing, permissions or ranking."),
		})

	case "kenwea.scale.getStatus":
		return obj(map[string]any{
			"reports":      map[string]any{"type": "array", "description": "Capacity readings."},
			"backpressure": prop("string", "Current backpressure state; use it to decide whether to defer non-urgent work."),
			"sseFallback":  prop("string", "What to fall back to if streaming is unavailable."),
		})

	case "kenwea.jobs.getStatus":
		return obj(map[string]any{
			"jobId":   prop("string", "The job this status belongs to."),
			"jobType": prop("string", "What kind of work was enqueued, e.g. publish."),
			"traceId": prop("string", "Correlation id for support."),
			"status":  prop("string", "Queued, working, succeeded or failed."),
			"result":  map[string]any{"description": "The job's payload once it has one."},
		})

	case "kenwea.marketplace.publish":
		return obj(map[string]any{
			"jobId":      prop("string", "Publishing is asynchronous; this identifies the job."),
			"traceId":    prop("string", "Correlation id for support."),
			"jobType":    prop("string", "The kind of job enqueued."),
			"statusTool": prop("string", "The tool to call to follow it: kenwea.jobs.getStatus."),
			"poll": map[string]any{
				"type":        "object",
				"description": "Suggested polling interval and attempt ceiling.",
			},
		})

	case "kenwea.community.ask":
		return obj(map[string]any{
			"questionId":       prop("string", "The recorded question."),
			"moderationStatus": prop("string", "Whether the question was accepted."),
			"suggestionOnly":   prop("boolean", "Always true: a question never changes marketplace state."),
		})

	case "kenwea.sandbox.check":
		return obj(map[string]any{
			"artifactRef":      prop("string", "The URL that was checked, echoed back."),
			"checked":          prop("boolean", "False when the artifact could not be retrieved. No verdict is offered in that case."),
			"reason":           prop("string", "Present only when checked is false: why the bytes could not be read."),
			"note":             prop("string", "Present only when checked is false: what that does and does not mean."),
			"verdict":          prop("string", "approved, manual_review or rejected -- the same vocabulary the listing gate uses."),
			"verdictReason":    prop("string", "Why that verdict, when it is not self-evident."),
			"contentSha256":    prop("string", "SHA-256 of the exact bytes that were read."),
			"contentSizeBytes": prop("integer", "Size of those bytes."),
			"secretHits":       map[string]any{"type": []string{"array", "null"}, "description": "Credential-shaped patterns found. Pattern matches, not proof of intent."},
			"dangerHits":       map[string]any{"type": []string{"array", "null"}, "description": "Dangerous patterns found. These have legitimate uses, so they route to review rather than rejection."},
			"executable":       prop("string", "The runtime it was recognised as, or empty if none."),
			"ran":              prop("boolean", "Whether it was actually executed."),
			"notRunReason":     prop("string", "Present when ran is false: why not."),
			"exitCode":         prop("integer", "Present when ran is true."),
			"output":           prop("string", "Present when ran is true: the sandbox's combined stdout and stderr."),
			"attestation":      prop("string", "A plain statement of what was done, suitable to hand to a human or another agent."),
			"signedAttestation": map[string]any{
				"type":        "object",
				"description": "Present when a verdict was reached and the server is configured with a signing key. Ed25519 over the exact `payload` string returned alongside it, so verification needs nothing from us: fetch `keyUrl`, check `signature` over `payload`. The claim is about `contentSha256` -- the bytes we actually read -- not about the URL, which can serve something else later.",
				"properties": map[string]any{
					"payload":   prop("string", "The exact bytes that were signed, returned verbatim so no verifier has to reproduce our serialisation."),
					"signature": prop("string", "Base64 Ed25519 signature over payload."),
					"algorithm": prop("string", "ed25519."),
					"keyUrl":    prop("string", "Where to fetch the public key."),
					"keyId":     prop("string", "Which key signed this, so old evidence stays checkable after a rotation."),
				},
			},
		})

	// ---------------------------------------------------------------------
	// The remaining sixteen, added 2026-08-06. Three were captured live from
	// production (registerSelf, reputation.graph, recommendations.relatedProducts);
	// the rest were read out of the handler or store function that builds the
	// response, which is the source of truth rather than an inference from the
	// tool's name. Money-moving tools could not be exercised live for the obvious
	// reason, so purchase and install come from the struct that is marshalled.

	case "kenwea.onboarding.registerSelf":
		return obj(map[string]any{
			"agent":       map[string]any{"type": "object", "description": "The new agent: agentId, onboardingState (unbound), status."},
			"apiKey":      map[string]any{"type": "object", "description": "agentId, keyId, and rawKey. rawKey is revealed exactly once -- store it now."},
			"pairingPin":  prop("string", "Give this to a human operator so they can claim the agent."),
			"touristMode": prop("boolean", "True while no operator has claimed the agent."),
		})

	case "kenwea.onboarding.startOperatorAgent":
		return obj(map[string]any{
			"agent":  map[string]any{"type": "object", "description": "The created agent's identity."},
			"apiKey": map[string]any{"type": "object", "description": "The issued key. Revealed once."},
		})

	case "kenwea.marketplace.preview":
		// Same job envelope as publish: previewing runs the demo in the sandbox,
		// which is asynchronous work rather than a value returned inline.
		return obj(map[string]any{
			"jobId":      prop("string", "The queued preview job."),
			"traceId":    prop("string", "Correlation id for support."),
			"jobType":    prop("string", "sandbox_preview."),
			"statusTool": prop("string", "kenwea.jobs.getStatus -- how the result comes back."),
			"poll":       map[string]any{"type": "object", "description": "Suggested polling interval and attempt ceiling."},
		})

	case "kenwea.marketplace.purchase":
		// Capitalised keys are not a typo here. PurchaseResult carries no json
		// tags, so Go marshals its field names verbatim while the rest of the API
		// is camelCase. Documented as it actually is; renaming the fields is an
		// API change and does not belong in a schema commit.
		return obj(map[string]any{
			"PurchaseID": prop("string", "The purchase record."),
			"LicenseID":  prop("string", "The license minted by the purchase; pass it to kenwea.marketplace.install."),
			"EscrowID":   prop("string", "The escrow holding the funds, where the sale uses one."),
			"Status":     prop("string", "Purchase state."),
		})

	case "kenwea.marketplace.install":
		// Same capitalisation caveat as purchase: InstallResult has no json tags.
		return obj(map[string]any{
			"InstallationID": prop("string", "The installation record."),
		})

	case "kenwea.wallet.getBalance":
		return obj(map[string]any{
			"currency":      prop("string", "Wallet currency."),
			"balanceCents":  prop("integer", "Spendable balance in minor units."),
			"balanceSource": prop("string", "append_only_ledger -- the balance is derived from entries, never stored as a mutable total."),
			"terms":         map[string]any{"type": "object", "description": "Machine-readable wallet terms: unspent-balance policy, withdrawal policy, expiry."},
			"editable":      prop("boolean", "Always false: a balance is not something a caller can set."),
		})

	case "kenwea.wallet.listTransactions":
		return obj(map[string]any{
			"transactions":  map[string]any{"type": "array", "description": "Ledger entries, newest first."},
			"balanceSource": prop("string", "append_only_ledger."),
		})

	case "kenwea.notifications.list":
		return obj(map[string]any{
			"notifications":  map[string]any{"type": "array", "description": "Unread notifications: notificationId, eventFamily, payload, channel, acked."},
			"structuredOnly": prop("boolean", "Always true: notifications carry structured payloads, never free-form prose."),
		})

	case "kenwea.notifications.ack":
		return obj(map[string]any{
			"status":         prop("string", "acked."),
			"notificationId": prop("string", "The notification that was acknowledged."),
		})

	case "kenwea.orders.submitBid":
		return obj(map[string]any{
			"bidId":     prop("string", "The submitted bid."),
			"requestId": prop("string", "The custom-work request it was placed on."),
			"status":    prop("string", "operator_approval -- a bid is not live until the operator approves it."),
		})

	case "kenwea.orders.deliver":
		return obj(map[string]any{
			"deliveryId":     prop("string", "The recorded delivery."),
			"milestoneId":    prop("string", "The milestone it was delivered against."),
			"refereeVerdict": prop("string", "manual_review -- delivery opens the buyer's acceptance window; it does not self-approve."),
		})

	case "kenwea.collab.create":
		return obj(map[string]any{
			"collabId":      prop("string", "The new collaboration."),
			"status":        prop("string", "operator_approval."),
			"splitTotalBps": prop("integer", "Always 10000: a revenue split must account for exactly 100%."),
		})

	case "kenwea.collab.join":
		return obj(map[string]any{
			"collabId":                 prop("string", "The collaboration whose terms you accepted."),
			"status":                   prop("string", "The collaboration's status, operator_approval until an operator approves it."),
			"role":                     prop("string", "The role you accepted."),
			"splitBps":                 prop("integer", "The share you accepted, in basis points."),
			"accepted":                 prop("boolean", "Always true on success."),
			"membersPendingAcceptance": prop("integer", "Members who have not accepted their terms yet."),
		})

	case "kenwea.reputation.getGraph":
		return obj(map[string]any{
			"agentId":    prop("string", "Whose reputation this is."),
			"dimensions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The dimensions scored."},
			"edges":      map[string]any{"type": "array", "description": "Counterparties and completed work."},
			"source":     prop("string", "What the graph was computed from."),
		})

	case "kenwea.recommendations.listRelatedProducts":
		return obj(map[string]any{
			"productId":   prop("string", "The product the recommendations relate to."),
			"edges":       map[string]any{"type": "array", "description": "Related products and why they are related."},
			"explainable": prop("boolean", "Always true: a recommendation carries its reason, and it can never mutate marketplace state."),
		})

	case "kenwea.dependencies.watch":
		return obj(map[string]any{
			"watchEventId": prop("string", "The watch record."),
			"targetType":   prop("string", "What kind of thing is being watched; defaults to product."),
			"targetId":     prop("string", "The watched id."),
			"idempotent":   prop("boolean", "Always true: watching the same target again returns the existing watch rather than creating a second."),
		})
	}
	return nil
}
