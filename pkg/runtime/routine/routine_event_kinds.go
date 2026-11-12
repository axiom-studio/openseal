package routine

import "github.com/axiom-studio/openseal/pkg/models"

// EventKinds lists every kind in a fixed order.
var EventKinds = []models.RoutineEventKind{
	models.RoutineEventSlackMessage,
	models.RoutineEventTelegramMessage,
	models.RoutineEventGitHub,
	models.RoutineEventGmailMessage,
	models.RoutineEventCalendar,
	models.RoutineEventWebhook,
}
