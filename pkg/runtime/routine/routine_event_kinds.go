package routine

import "github.com/axiom-studio/openseal/pkg/models"

// RoutineEventKinds lists every kind in a fixed order.
var RoutineEventKinds = []models.RoutineEventKind{
	models.RoutineEventSlackMessage,
	models.RoutineEventTelegramMessage,
	models.RoutineEventGitHub,
	models.RoutineEventGmailMessage,
	models.RoutineEventCalendar,
	models.RoutineEventWebhook,
}
