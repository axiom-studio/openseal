package presentation

func clockSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": clockTimePattern}
}

func dateSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": isoDate.String()}
}

func ratingSchema() map[string]interface{} {
	return objectOf([]interface{}{"value"}, map[string]interface{}{
		"value": map[string]interface{}{"type": "number", "minimum": 0, "maximum": maxRating}, "count": map[string]interface{}{"type": "integer", "minimum": 0},
	})
}

func agendaSchema() map[string]interface{} {
	item := objectOf([]interface{}{"id", "start", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "start": clockSchema(), "end": clockSchema(), "title": filledText(), "place": freeText(), "tone": enum(toneNames()),
	})
	day := objectOf([]interface{}{"date"}, map[string]interface{}{"date": dateSchema(), "label": freeText(), "items": array(item, 0, maxDayItems)})
	slot := objectOf([]interface{}{"date", "start", "end"}, map[string]interface{}{"date": dateSchema(), "start": clockSchema(), "end": clockSchema()})
	return objectOf([]interface{}{"days"}, map[string]interface{}{
		"timezone": describe(str(maxTimezoneRunes), ruleTimezone), "days": array(day, 1, maxAgendaDays), "freeSlots": array(slot, 0, maxFreeSlots),
	})
}

func itinerarySchema() map[string]interface{} {
	travel := objectOf([]interface{}{"mode", "minutes"}, map[string]interface{}{"mode": enum(TravelModes), "minutes": integer(1, MaxTravelMinutes)})
	item := objectOf([]interface{}{"id", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "time": clockSchema(), "title": filledText(), "place": freeText(), "detail": freeText(),
		"image": imageRefSchema(), "travel": travel,
		"location": placeIDSchema(), "lat": false, "lon": false,
	})
	day := objectOf([]interface{}{"id", "label", "items"}, map[string]interface{}{
		"id": itemIDSchema(), "label": filledText(), "date": dateSchema(), "items": array(item, 1, maxDayItems),
	})
	return objectOf([]interface{}{"days"}, map[string]interface{}{"days": array(day, 1, maxItineraryDays)})
}

func placeSchema() map[string]interface{} {
	return objectOf(nil, map[string]interface{}{
		"name": filledText(), "category": freeText(), "image": imageRefSchema(), "address": freeText(),
		"rating": ratingSchema(), "priceLevel": integer(1, maxPriceLevel), "hours": shownList(filledText(), 0, maxHours),
		"facts":    shownList(filledText(), 0, maxFacts),
		"location": placeIDSchema(), "lat": false, "lon": false,
	})
}

func breakdownSchema() map[string]interface{} {
	item := objectOf([]interface{}{"id", "label", "value"}, map[string]interface{}{
		"id": itemIDSchema(), "label": filledText(), "value": map[string]interface{}{"type": "number", "minimum": 0}, "icon": iconSchema(),
	})
	return objectOf([]interface{}{"kind", "items"}, map[string]interface{}{
		"kind": enum(BreakdownKinds), "format": enum(formatNames()), "currency": currencySchema(), "unit": freeText(),
		"items": array(item, minBreakdownItems, maxBreakdownItems),
	})
}

func progressSchema() map[string]interface{} {
	item := objectOf([]interface{}{"id", "label", "value", "target"}, map[string]interface{}{
		"id": itemIDSchema(), "label": filledText(), "value": map[string]interface{}{"type": "number", "minimum": 0},
		"target": map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "format": enum(formatNames()), "currency": currencySchema(), "unit": freeText(),
	})
	return objectOf([]interface{}{"kind", "items"}, map[string]interface{}{"kind": enum(ProgressKinds), "items": array(item, 1, maxProgressItems)})
}

func pollSchema() map[string]interface{} {
	option := objectOf([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": filledText(), "image": imageRefSchema()})
	return objectOf([]interface{}{"question", "options"}, map[string]interface{}{
		"question": filledText(), "options": array(option, minPollOptions, maxPollOptions), "multiple": boolean(),
		"closesAt": describe(map[string]interface{}{"type": "string"}, rulePollClosesAt),
	})
}

func explorerSchema() map[string]interface{} {
	expression := strRange(1, MaxExplorerExprRunes)
	input := objectOf([]interface{}{"id", "label", "kind", "default"}, map[string]interface{}{
		"id": map[string]interface{}{"type": "string", "pattern": explorerNamePattern}, "label": filledText(),
		"kind": describe(enum(ExplorerInputKinds), ruleExplorerInputs),
		"min":  number(), "max": number(), "step": map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "default": number(), "unit": freeText(), "format": enum(formatNames()), "currency": currencySchema(),
	})
	output := objectOf([]interface{}{"id", "label", "expr"}, map[string]interface{}{
		"id": itemIDSchema(), "label": filledText(), "expr": expression, "format": enum(formatNames()), "currency": currencySchema(),
		"unit":   freeText(),
		"digits": integer(0, MaxExplorerDigits),
	})
	series := objectOf([]interface{}{"label", "expr"}, map[string]interface{}{"label": filledText(), "expr": expression})
	chart := objectOf([]interface{}{"x", "series"}, map[string]interface{}{
		"x":      describe(map[string]interface{}{"type": "string"}, ruleExplorerX),
		"t":      describe(objectOf([]interface{}{"from", "to"}, map[string]interface{}{"from": number(), "to": number(), "label": freeText()}), ruleExplorerT),
		"points": integer(2, MaxExplorerPoints), "series": array(series, 1, maxExplorerSeries),
	})
	return objectOf([]interface{}{"inputs", "outputs"}, map[string]interface{}{
		"inputs": array(input, 1, maxExplorerInputs), "outputs": array(output, 1, maxExplorerOutputs), "chart": chart,
	})
}

func routineSchema() map[string]interface{} {
	item := objectOf([]interface{}{"label", "text"}, map[string]interface{}{"label": filledText(), "text": filledText()})
	return objectOf(nil, map[string]interface{}{"items": shownList(item, 0, maxRoutineItems)})
}

func tasksSchema() map[string]interface{} {
	binding := objectOf([]interface{}{"kind", "id"}, map[string]interface{}{"kind": enum([]string{string(BindingConversationTask)}), "id": idSchema()})
	row := objectOf([]interface{}{"binding", "label"}, map[string]interface{}{"binding": binding, "label": filledText()})
	return objectOf([]interface{}{"rows"}, map[string]interface{}{"rows": array(row, 1, maxTaskRows)})
}

func agentCardSchema() map[string]interface{} {
	return objectOf([]interface{}{"headline"}, map[string]interface{}{
		"headline": filledText(), "points": shownList(filledText(), 0, maxAgentPoints),
	})
}

func placeIDSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": PlaceIDPattern}
}

func mapSchema() map[string]interface{} {
	pin := objectOf([]interface{}{"id", "place"}, map[string]interface{}{
		"id": itemIDSchema(), "place": placeIDSchema(), "label": freeText(), "detail": freeText(),
		// The host fills these in from find_place.
		"lat": false, "lon": false, "address": false,
	})
	return objectOf([]interface{}{"pins"}, map[string]interface{}{
		"pins": array(pin, 1, MaxMapPins), "connect": describe(boolean(), "Dashed line through the pins in visit order."),
	})
}
