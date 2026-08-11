// Package evalfixture provides the deterministic ten-server, 200-tool MCP
// federation and its single-server aggregate used to evaluate deferred tool
// discovery with real transports.
package evalfixture

// Domain describes one independent evaluation MCP server.
type Domain struct {
	Name  string
	Tools []ToolDefinition
}

// ToolDefinition is a realistic deterministic evaluation operation.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Mutating    bool   `json:"mutating"`
}

// AggregateTool identifies one domain tool on the single aggregate MCP server.
// Name is unique within that server while Definition retains the logical
// domain operation used by the deterministic handler and oracle.
type AggregateTool struct {
	Domain     string
	Name       string
	Definition ToolDefinition
}

// AggregateTools returns all 200 federation tools with domain-qualified names
// suitable for exposure by one MCP server.
func AggregateTools() []AggregateTool {
	tools := make([]AggregateTool, 0, 200)
	for _, domain := range Domains() {
		for _, definition := range domain.Tools {
			tools = append(tools, AggregateTool{
				Domain:     domain.Name,
				Name:       domain.Name + "__" + definition.Name,
				Definition: definition,
			})
		}
	}
	return tools
}

// Domains returns ten servers with exactly twenty realistic tools each.
func Domains() []Domain {
	return []Domain{
		domain("source_control", []string{"get_repository", "list_branches", "create_branch", "delete_branch", "get_pull_request", "list_pull_requests", "create_pull_request", "update_pull_request", "merge_pull_request", "close_pull_request", "get_reviews", "request_review", "submit_review", "get_check_runs", "rerun_check", "list_commits", "get_commit", "compare_commits", "search_code", "create_release"}),
		domain("issue_tracker", []string{"search_issues", "get_issue", "list_issues", "create_issue", "update_issue", "close_issue", "reopen_issue", "add_issue_comment", "list_issue_comments", "assign_issue", "add_label", "remove_label", "list_projects", "get_project", "list_project_items", "move_project_item", "list_milestones", "create_milestone", "set_issue_milestone", "link_related_issue"}),
		domain("team_chat", []string{"search_messages", "get_message", "send_channel_message", "send_direct_message", "update_message", "delete_message", "reply_thread", "get_thread", "list_channels", "get_channel", "create_channel", "archive_channel", "invite_to_channel", "remove_from_channel", "list_members", "get_member", "set_channel_topic", "add_reaction", "remove_reaction", "upload_attachment"}),
		domain("docs_drive", []string{"search_files", "get_file_metadata", "read_document", "create_document", "update_document", "delete_document", "move_file", "copy_file", "share_document", "revoke_share", "list_folder", "create_folder", "rename_file", "download_file", "upload_file", "list_revisions", "get_revision", "restore_revision", "add_document_comment", "resolve_document_comment"}),
		domain("observability", []string{"search_errors", "get_issue_details", "list_issue_events", "resolve_issue", "reopen_issue", "query_logs", "get_log_context", "query_metrics", "list_metric_names", "create_dashboard", "get_dashboard", "list_incidents", "get_incident", "create_incident", "update_incident", "resolve_incident", "list_alert_rules", "create_alert_rule", "mute_alert_rule", "get_trace"}),
		domain("sql_analytics", []string{"list_databases", "get_database", "list_schemas", "list_tables", "describe_table", "list_columns", "query_readonly", "execute_statement", "explain_query", "analyze_query", "list_indexes", "create_index", "drop_index", "list_views", "create_view", "drop_view", "get_table_stats", "export_query_results", "begin_transaction", "rollback_transaction"}),
		domain("cloud_infra", []string{"list_services", "get_service", "create_service", "update_service", "delete_service", "get_deployment", "list_deployments", "deploy_service", "rollback_deployment", "restart_service", "scale_service", "get_logs", "list_jobs", "run_job", "cancel_job", "list_environments", "get_environment", "set_environment_variable", "list_secret_names", "rotate_secret"}),
		domain("browser_web", []string{"navigate", "go_back", "go_forward", "reload", "snapshot", "take_screenshot", "click", "double_click", "hover", "fill", "select_option", "press_key", "upload_file", "download", "evaluate", "wait_for_selector", "get_text", "get_attribute", "list_tabs", "close_tab"}),
		domain("commerce_crm", []string{"search_customers", "get_customer", "create_customer", "update_customer", "list_orders", "get_order", "create_order", "cancel_order", "fulfill_order", "list_order_items", "add_order_note", "issue_refund", "get_refund", "list_invoices", "get_invoice", "send_invoice", "list_support_cases", "get_support_case", "update_support_case", "close_support_case"}),
		domain("calendar_mail", []string{"search_mail", "read_message", "send_message", "reply_message", "forward_message", "archive_message", "delete_message", "list_mail_folders", "move_message", "list_events", "get_event", "create_event", "update_event", "cancel_event", "reschedule_event", "list_calendars", "check_availability", "add_event_attendee", "remove_event_attendee", "create_draft"}),
	}
}

func domain(name string, names []string) Domain {
	tools := make([]ToolDefinition, 0, len(names))
	for _, toolName := range names {
		mutating := isMutating(toolName)
		description := describe(name, toolName, mutating)
		tools = append(tools, ToolDefinition{Name: toolName, Description: description, Mutating: mutating})
	}
	return Domain{Name: name, Tools: tools}
}

func isMutating(name string) bool {
	for _, prefix := range []string{"create_", "update_", "delete_", "merge_", "close_", "reopen_", "add_", "remove_", "assign_", "set_", "send_", "reply_", "archive_", "invite_", "upload_", "resolve_", "move_", "copy_", "rename_", "download_", "restore_", "submit_", "rerun_", "revoke_", "mute_", "execute_", "analyze_", "drop_", "begin_", "rollback_", "deploy_", "restart_", "scale_", "run_", "cancel_", "rotate_", "navigate", "go_", "reload", "click", "double_", "hover", "fill", "select_", "press_", "evaluate", "wait_", "fulfill_", "issue_", "forward_", "reschedule_"} {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func describe(domainName, toolName string, mutating bool) string {
	if specific := specificDescription(domainName, toolName); specific != "" {
		return specific + " Accepts realistic identifiers, filters, pagination, and optional nested metadata; mutations affect only the disposable evaluation workspace."
	}
	kind := "Read"
	if mutating {
		kind = "Update"
	}
	return kind + " deterministic " + humanize(domainName) + " fixture data using the operation " + humanize(toolName) + ". Accepts realistic identifiers, filters, pagination, and optional nested metadata; mutations affect only the disposable evaluation workspace."
}

func specificDescription(domainName, toolName string) string {
	descriptions := map[string]string{
		"source_control/get_pull_request":   "Read pull request metadata, source and target branches, author, merge state, and current status.",
		"source_control/get_reviews":        "Retrieve pull request reviews, reviewers, approval decisions, requested changes, and required review state.",
		"source_control/get_check_runs":     "Retrieve CI check runs and required status checks for a pull request commit.",
		"source_control/merge_pull_request": "Merge an eligible pull request after required reviews and CI status checks pass.",
		"issue_tracker/search_issues":       "Search project issue tickets by natural language, labels, assignee, milestone, and open or closed status.",
		"issue_tracker/get_issue":           "Read one project issue ticket including its body, state, labels, assignee, and milestone.",
		"team_chat/search_messages":         "Search historical team chat messages across channels and direct conversations.",
		"team_chat/get_thread":              "Read a complete team chat message thread and all replies.",
		"docs_drive/read_document":          "Read the complete text and metadata of a document in shared drive storage.",
		"docs_drive/share_document":         "Grant a user or group access to a shared drive document with a selected role.",
		"observability/search_errors":       "Search application monitoring error events, grouped failures, and unresolved error issues; not project tickets.",
		"observability/get_issue_details":   "Read monitoring issue details, stack traces, affected releases, and event counts.",
		"observability/query_logs":          "Query indexed observability logs with service, request, severity, and time filters.",
		"sql_analytics/query_readonly":      "Execute a read-only SQL SELECT query and return rows without changing database state.",
		"sql_analytics/describe_table":      "Describe a SQL table's columns, data types, nullability, keys, and constraints without executing a statement.",
		"sql_analytics/explain_query":       "Explain a SQL query execution plan without executing a mutating statement.",
		"cloud_infra/get_logs":              "Read direct runtime logs for a deployed cloud service, task, or job.",
		"cloud_infra/get_deployment":        "Inspect one cloud deployment by identifier, including service, environment, version, and rollout status.",
		"cloud_infra/deploy_service":        "Deploy a selected version or image to a cloud service and environment.",
		"browser_web/navigate":              "Navigate the active browser tab to a URL and wait for document readiness.",
		"browser_web/snapshot":              "Capture a structured accessibility and DOM snapshot of the current browser page.",
		"browser_web/take_screenshot":       "Capture a visual screenshot image of the current browser page or element.",
		"docs_drive/upload_file":            "Upload a local file into shared document and drive storage.",
		"commerce_crm/search_customers":     "Search CRM customer records by name, email address, phone, or external identifier.",
		"commerce_crm/get_order":            "Read one commerce order, fulfillment status, payment state, customer, and line items.",
		"commerce_crm/issue_refund":         "Issue a payment refund for an eligible paid commerce order.",
		"calendar_mail/search_mail":         "Search email messages by sender, recipient, subject, folder, and date.",
		"calendar_mail/send_message":        "Send an email message to recipients with subject, body, and optional attachments.",
		"calendar_mail/list_events":         "List calendar events in a date range, including tomorrow and upcoming meetings.",
		"calendar_mail/check_availability":  "Check free and busy availability for people and calendars over a requested time window without creating an event.",
		"calendar_mail/reschedule_event":    "Reschedule an existing calendar event to a new start and end time.",
	}
	return descriptions[domainName+"/"+toolName]
}

func humanize(value string) string {
	out := make([]byte, len(value))
	for i := range value {
		if value[i] == '_' {
			out[i] = ' '
		} else {
			out[i] = value[i]
		}
	}
	return string(out)
}
