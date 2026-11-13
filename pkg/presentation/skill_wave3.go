package presentation

func clockSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": clockTimePattern}
}

func dateSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": isoDate.String()}
}

func ratingSchema() map[string]interface{} {
	return closedObject([]interface{}{"value"}, map[string]interface{}{
		"value": map[string]interface{}{"type": "number", "minimum": 0, "maximum": maxRating}, "count": map[string]interface{}{"type": "integer", "minimum": 0},
	})
}

func agendaSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "start", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "start": clockSchema(), "end": clockSchema(), "title": strRange(1, maxPlanTitleRunes), "place": str(maxPlanPlaceRunes), "tone": enum(toneNames()),
	})
	day := closedObject([]interface{}{"date"}, map[string]interface{}{"date": dateSchema(), "label": str(maxDayLabelRunes), "items": array(item, 0, maxDayItems)})
	slot := closedObject([]interface{}{"date", "start", "end"}, map[string]interface{}{"date": dateSchema(), "start": clockSchema(), "end": clockSchema()})
	return closedObject([]interface{}{"days"}, map[string]interface{}{
		"timezone": describe(str(maxTimezoneRunes), "IANA name, shown only."), "days": array(day, 1, maxAgendaDays), "freeSlots": array(slot, 0, maxFreeSlots),
	})
}

func itinerarySchema() map[string]interface{} {
	travel := closedObject([]interface{}{"mode", "minutes"}, map[string]interface{}{"mode": enum(TravelModes), "minutes": integer(1, MaxTravelMinutes)})
	item := closedObject([]interface{}{"id", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "time": clockSchema(), "title": strRange(1, maxPlanTitleRunes), "place": str(maxPlanPlaceRunes), "detail": str(maxItineraryDetailRunes),
		"image": imageRefSchema(), "travel": travel,
		"location": placeIDSchema(),
	})
	day := closedObject([]interface{}{"id", "label", "items"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, maxDayLabelRunes), "date": dateSchema(), "items": array(item, 1, maxDayItems),
	})
	return closedObject([]interface{}{"days"}, map[string]interface{}{"days": array(day, 1, maxItineraryDays)})
}

func placeSchema() map[string]interface{} {
	return closedObject(nil, map[string]interface{}{
		"name": strRange(1, maxPlaceNameRunes), "category": str(maxPlaceCategoryRunes), "image": imageRefSchema(), "address": str(maxStampedAddressRunes),
		"rating": ratingSchema(), "priceLevel": integer(1, maxPriceLevel), "hours": array(strRange(1, maxHoursRunes), 0, maxHours),
		"facts":    array(strRange(1, maxFactRunes), 0, maxFacts),
		"location": placeIDSchema(),
	})
}

func breakdownSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "label", "value"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, maxPartLabelRunes), "value": map[string]interface{}{"type": "number", "minimum": 0}, "icon": iconSchema(),
	})
	return closedObject([]interface{}{"kind", "items"}, map[string]interface{}{
		"kind": enum(BreakdownKinds), "format": enum(formatNames()), "currency": currencySchema(), "unit": str(maxUnitRunes),
		"items": array(item, minBreakdownItems, maxBreakdownItems),
	})
}

func progressSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "label", "value", "target"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, maxPartLabelRunes), "value": map[string]interface{}{"type": "number", "minimum": 0},
		"target": map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "format": enum(formatNames()), "currency": currencySchema(), "unit": str(maxUnitRunes),
	})
	return closedObject([]interface{}{"kind", "items"}, map[string]interface{}{"kind": enum(ProgressKinds), "items": array(item, 1, maxProgressItems)})
}

func pollSchema() map[string]interface{} {
	option := closedObject([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": strRange(1, maxPollLabelRunes), "image": imageRefSchema()})
	return closedObject([]interface{}{"question", "options"}, map[string]interface{}{
		"question": strRange(1, maxPollQuestionRunes), "options": array(option, minPollOptions, maxPollOptions), "multiple": boolean(),
		"closesAt": describe(map[string]interface{}{"type": "string"}, "RFC 3339 time voting ends."),
	})
}

func explorerSchema() map[string]interface{} {
	expression := strRange(1, MaxExplorerExprRunes)
	input := closedObject([]interface{}{"id", "label", "kind", "default"}, map[string]interface{}{
		"id": map[string]interface{}{"type": "string", "pattern": explorerNamePattern}, "label": strRange(1, maxExplorerLabelRunes),
		"kind": describe(enum(ExplorerInputKinds), "slider and stepper: min below max, step above 0, default on a step; toggle: default 0 or 1, no min, max or step."),
		"min":  number(), "max": number(), "step": map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "default": number(), "unit": str(maxExplorerUnitRunes), "format": enum(formatNames()), "currency": currencySchema(),
	})
	output := closedObject([]interface{}{"id", "label", "expr"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, maxExplorerLabelRunes), "expr": expression, "format": enum(formatNames()), "currency": currencySchema(),
		"unit":   str(maxExplorerUnitRunes),
		"digits": integer(0, MaxExplorerDigits),
	})
	series := closedObject([]interface{}{"label", "expr"}, map[string]interface{}{"label": strRange(1, maxExplorerSeriesRunes), "expr": expression})
	chart := closedObject([]interface{}{"x", "series"}, map[string]interface{}{
		"x":      describe(map[string]interface{}{"type": "string"}, "An input id (swept min to max) or t."),
		"t":      describe(closedObject([]interface{}{"from", "to"}, map[string]interface{}{"from": number(), "to": number(), "label": str(maxExplorerSeriesRunes)}), "Required when x is t; series may use t."),
		"points": integer(2, MaxExplorerPoints), "series": array(series, 1, maxExplorerSeries),
	})
	return closedObject([]interface{}{"inputs", "outputs"}, map[string]interface{}{
		"inputs": array(input, 1, maxExplorerInputs), "outputs": array(output, 1, maxExplorerOutputs), "chart": chart,
	})
}

func routineSchema() map[string]interface{} {
	item := closedObject([]interface{}{"label", "text"}, map[string]interface{}{"label": strRange(1, maxRoutineLabelRunes), "text": strRange(1, maxRoutineTextRunes)})
	return closedObject(nil, map[string]interface{}{"items": array(item, 0, maxRoutineItems)})
}

func tasksSchema() map[string]interface{} {
	binding := closedObject([]interface{}{"kind", "id"}, map[string]interface{}{"kind": enum([]string{string(BindingConversationTask)}), "id": idSchema()})
	row := closedObject([]interface{}{"binding", "label"}, map[string]interface{}{"binding": binding, "label": strRange(1, maxTaskLabelRunes)})
	return closedObject([]interface{}{"rows"}, map[string]interface{}{"rows": array(row, 1, maxTaskRows)})
}

func agentCardSchema() map[string]interface{} {
	return closedObject([]interface{}{"headline"}, map[string]interface{}{
		"headline": strRange(1, maxAgentLineRunes), "points": array(strRange(1, maxAgentLineRunes), 0, maxAgentPoints),
	})
}

func placeIDSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": PlaceIDPattern}
}

func mapSchema() map[string]interface{} {
	pin := closedObject([]interface{}{"id", "place"}, map[string]interface{}{
		"id": itemIDSchema(), "place": placeIDSchema(), "label": str(MaxMapPinLabelRunes), "detail": str(MaxMapPinDetail),
	})
	return closedObject([]interface{}{"pins"}, map[string]interface{}{
		"pins": array(pin, 1, MaxMapPins), "connect": describe(boolean(), "Dashed line through the pins in visit order."),
	})
}
