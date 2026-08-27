package connector

import "encoding/json"

const DelegateToolDescription = `Start exactly one ACP subordinate invocation, continue or cancel one owned invocation, or replay bounded native history for explicit owned completed subordinate targets. start accepts exactly one provider or owned child_session_id, one opaque prompt, and mode answer or follow. next_answer has no target fields and returns one completion in observed terminal order. follow_next and cancel require the exact owned child_session_id and turn_token. replay accepts only explicit child_session_ids and optional n, which defaults to 1 independently per target. When more is true, invoke the returned next_operation with exactly next_arguments.`

var delegateInputSchema = json.RawMessage(`{
  "type":"object",
  "oneOf":[
    {
      "type":"object",
      "additionalProperties":false,
      "required":["operation","provider","prompt","mode"],
      "properties":{
        "operation":{"const":"start"},
        "provider":{"type":"string","minLength":1,"maxLength":128},
        "prompt":{"type":"string"},
        "mode":{"enum":["answer","follow"]}
      }
    },
    {
      "type":"object",
      "additionalProperties":false,
      "required":["operation","child_session_id","prompt","mode"],
      "properties":{
        "operation":{"const":"start"},
        "child_session_id":{"type":"string","minLength":1,"maxLength":256},
        "prompt":{"type":"string"},
        "mode":{"enum":["answer","follow"]}
      }
    },
    {
      "type":"object",
      "additionalProperties":false,
      "required":["operation"],
      "properties":{"operation":{"const":"next_answer"}}
    },
    {
      "type":"object",
      "additionalProperties":false,
      "required":["operation","child_session_id","turn_token"],
      "properties":{
        "operation":{"const":"follow_next"},
        "child_session_id":{"type":"string","minLength":1,"maxLength":256},
        "turn_token":{"type":"string","minLength":1,"maxLength":256}
      }
    },
    {
      "type":"object",
      "additionalProperties":false,
      "required":["operation","child_session_id","turn_token"],
      "properties":{
        "operation":{"const":"cancel"},
        "child_session_id":{"type":"string","minLength":1,"maxLength":256},
        "turn_token":{"type":"string","minLength":1,"maxLength":256}
      }
    },
    {
      "type":"object",
      "additionalProperties":false,
      "required":["operation","child_session_ids"],
      "properties":{
        "operation":{"const":"replay"},
        "child_session_ids":{
          "type":"array",
          "minItems":1,
          "maxItems":16,
          "uniqueItems":true,
          "items":{"type":"string","minLength":1,"maxLength":256}
        },
        "n":{"type":"integer","minimum":1,"maximum":32}
      }
    }
  ]
}`)

func DelegateCapability() DynamicTool {
	return DynamicTool{
		Type:        "function",
		Name:        DelegateToolName,
		Description: DelegateToolDescription,
		InputSchema: append(json.RawMessage(nil), delegateInputSchema...),
	}
}
