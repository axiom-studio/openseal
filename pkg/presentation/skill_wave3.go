package presentation

func clockSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": clockTimePattern}
}

func dateSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": isoDate.String()}
}

func ratingSchema() map[string]interface{} {
	return closedObject([]interface{}{"value"}, map[string]interface{}{
		"value": map[string]interface{}{"type": "number", "minimum": 0, "maximum": 5}, "count": map[string]interface{}{"type": "integer", "minimum": 0},
	})
}

func agendaSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "start", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "start": clockSchema(), "end": clockSchema(), "title": strRange(1, 80), "place": str(80), "tone": enum(toneNames()),
	})
	day := closedObject([]interface{}{"date", "items"}, map[string]interface{}{"date": dateSchema(), "label": str(40), "items": array(item, 0, 12)})
	slot := closedObject([]interface{}{"date", "start", "end"}, map[string]interface{}{"date": dateSchema(), "start": clockSchema(), "end": clockSchema()})
	return closedObject([]interface{}{"days"}, map[string]interface{}{
		"timezone": describe(str(64), "IANA name, shown only."), "days": array(day, 1, 7), "freeSlots": array(slot, 0, 12),
	})
}

func itinerarySchema() map[string]interface{} {
	travel := closedObject([]interface{}{"mode", "minutes"}, map[string]interface{}{"mode": enum(TravelModes), "minutes": integer(1, MaxTravelMinutes)})
	item := closedObject([]interface{}{"id", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "time": clockSchema(), "title": strRange(1, 80), "place": str(80), "detail": str(200), "image": imageRefSchema(), "travel": travel,
	})
	day := closedObject([]interface{}{"id", "label", "items"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, 40), "date": dateSchema(), "items": array(item, 1, 12),
	})
	return closedObject([]interface{}{"days"}, map[string]interface{}{"days": array(day, 1, 10)})
}

func placeSchema() map[string]interface{} {
	return closedObject([]interface{}{"name"}, map[string]interface{}{
		"name": strRange(1, 80), "category": str(30), "image": imageRefSchema(), "address": str(160), "rating": ratingSchema(),
		"priceLevel": integer(1, 4), "hours": array(strRange(1, 40), 0, 7), "facts": array(strRange(1, 60), 0, 4),
	})
}

func breakdownSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "label", "value"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, 40), "value": map[string]interface{}{"type": "number", "minimum": 0}, "icon": iconSchema(),
	})
	return closedObject([]interface{}{"kind", "items"}, map[string]interface{}{
		"kind": enum(BreakdownKinds), "format": enum(formatNames()), "currency": currencySchema(), "unit": str(24), "items": array(item, 2, 12),
	})
}

func progressSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "label", "value", "target"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, 40), "value": map[string]interface{}{"type": "number", "minimum": 0},
		"target": map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "format": enum(formatNames()), "currency": currencySchema(), "unit": str(24),
	})
	return closedObject([]interface{}{"kind", "items"}, map[string]interface{}{"kind": enum(ProgressKinds), "items": array(item, 1, 6)})
}

func pollSchema() map[string]interface{} {
	option := closedObject([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": strRange(1, 60), "image": imageRefSchema()})
	return closedObject([]interface{}{"question", "options"}, map[string]interface{}{
		"question": strRange(1, 200), "options": array(option, 2, 8), "multiple": boolean(),
		"closesAt": describe(map[string]interface{}{"type": "string"}, "RFC 3339 time voting ends."),
	})
}

func explorerSchema() map[string]interface{} {
	expression := strRange(1, MaxExplorerExprRunes)
	input := closedObject([]interface{}{"id", "label", "kind", "default"}, map[string]interface{}{
		"id": map[string]interface{}{"type": "string", "pattern": explorerNamePattern}, "label": strRange(1, 40),
		"kind": describe(enum(ExplorerInputKinds), "slider and stepper: min below max, step above 0, default on a step; toggle: default 0 or 1, no min, max or step."),
		"min":  number(), "max": number(), "step": number(), "default": number(), "unit": str(16), "format": enum(formatNames()), "currency": currencySchema(),
	})
	output := closedObject([]interface{}{"id", "label", "expr"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, 40), "expr": expression, "format": enum(formatNames()), "currency": currencySchema(), "unit": str(16),
		"digits": integer(0, MaxExplorerDigits),
	})
	series := closedObject([]interface{}{"label", "expr"}, map[string]interface{}{"label": strRange(1, 30), "expr": expression})
	chart := closedObject([]interface{}{"x", "series"}, map[string]interface{}{
		"x":      describe(map[string]interface{}{"type": "string"}, "An input id (swept min to max) or t."),
		"t":      describe(closedObject([]interface{}{"from", "to"}, map[string]interface{}{"from": number(), "to": number(), "label": str(30)}), "Required when x is t; series may use t."),
		"points": integer(2, MaxExplorerPoints), "series": array(series, 1, 3),
	})
	return closedObject([]interface{}{"inputs", "outputs"}, map[string]interface{}{"inputs": array(input, 1, 4), "outputs": array(output, 1, 4), "chart": chart})
}

func routineSchema() map[string]interface{} {
	item := closedObject([]interface{}{"label", "text"}, map[string]interface{}{"label": strRange(1, 40), "text": strRange(1, 300)})
	return closedObject(nil, map[string]interface{}{"items": array(item, 0, 14)})
}

func tasksSchema() map[string]interface{} {
	binding := closedObject([]interface{}{"kind", "id"}, map[string]interface{}{"kind": enum([]string{string(BindingConversationTask)}), "id": idSchema()})
	row := closedObject([]interface{}{"binding", "label"}, map[string]interface{}{"binding": binding, "label": strRange(1, 80)})
	return closedObject([]interface{}{"rows"}, map[string]interface{}{"rows": array(row, 1, 12)})
}

func agentCardSchema() map[string]interface{} {
	return closedObject([]interface{}{"headline"}, map[string]interface{}{"headline": strRange(1, 80), "points": array(strRange(1, 80), 0, 4)})
}
