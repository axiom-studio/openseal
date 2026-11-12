package runtime

// RoutineEventKinds lists every kind in a fixed order.
var RoutineEventKinds = []RoutineEventKind{
	RoutineEventSlackMessage,
	RoutineEventTelegramMessage,
	RoutineEventGitHub,
	RoutineEventGmailMessage,
	RoutineEventCalendar,
	RoutineEventWebhook,
}
