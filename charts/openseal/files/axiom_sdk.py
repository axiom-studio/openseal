"""
Axiom Python SDK for Code Nodes

This SDK provides automatic tool injection for code execution nodes.
Tools are injected as callable functions into the global namespace.

Usage:
    # Tools are automatically available in code nodes:
    results = vector_search(query="search term", limit=5)
"""

import json
import urllib.request
import urllib.error


class ToolExecutionError(Exception):
    """Raised when a tool execution fails"""
    pass


def inject_tools(globals_dict, tools_config):
    """
    Inject tool functions into the global namespace.
    
    Args:
        globals_dict: The global namespace dictionary to inject into
        tools_config: Configuration dict with 'api_base', 'token', and 'tools'
    
    Example tools_config:
        {
            "api_base": "http://cortex-api/api/v1/agent/executions/run-123/nodes/node-456/tools/invoke",
            "token": "eyJ...",
            "tools": [
                {"name": "vector_search", "description": "Search vector database"},
                {"name": "slack_send", "description": "Send Slack message"}
            ]
        }
    """
    api_base = tools_config.get('api_base')
    token = tools_config.get('token')
    tools = tools_config.get('tools', [])
    
    if not api_base or not token:
        raise ValueError("tools_config must contain 'api_base' and 'token'")
    
    # Inject each tool as a callable function
    for tool in tools:
        tool_name = tool.get('name')
        if not tool_name:
            continue
        
        # Create tool function and inject it
        tool_func = _make_tool_caller(api_base, token, tool_name)
        tool_func.__name__ = tool_name
        tool_func.__doc__ = tool.get('description', f"Call {tool_name} tool")
        
        globals_dict[tool_name] = tool_func


def _make_tool_caller(api_base, token, tool_name):
    """
    Factory function that creates a tool caller function.
    
    Args:
        api_base: Base URL for tool proxy API
        token: JWT authentication token
        tool_name: Name of the tool to call
    
    Returns:
        A function that calls the tool with given arguments
    """
    def tool_caller(**kwargs):
        """
        Call a tool with the given arguments.
        
        Args:
            **kwargs: Arguments to pass to the tool
        
        Returns:
            The tool execution result
        
        Raises:
            ToolExecutionError: If the tool execution fails
        """
        # Build request payload
        payload = {
            "tool_name": tool_name,
            "arguments": kwargs
        }
        
        # Prepare HTTP request
        payload_json = json.dumps(payload).encode('utf-8')
        
        req = urllib.request.Request(
            api_base,
            data=payload_json,
            headers={
                'Content-Type': 'application/json',
                'Authorization': f'Bearer {token}'
            },
            method='POST'
        )
        
        try:
            # Make HTTP request to tool proxy
            with urllib.request.urlopen(req, timeout=300) as response:
                response_data = response.read().decode('utf-8')
                result = json.loads(response_data)
                
                # Check if execution was successful
                if result.get('success'):
                    return result.get('result')
                else:
                    error_msg = result.get('error', 'Unknown error')
                    raise ToolExecutionError(f"{tool_name} failed: {error_msg}")
        
        except urllib.error.HTTPError as e:
            error_body = e.read().decode('utf-8') if e.fp else str(e)
            try:
                error_json = json.loads(error_body)
                error_msg = error_json.get('error', error_body)
            except:
                error_msg = error_body
            raise ToolExecutionError(f"{tool_name} HTTP error {e.code}: {error_msg}")
        
        except urllib.error.URLError as e:
            raise ToolExecutionError(f"{tool_name} network error: {e.reason}")
        
        except json.JSONDecodeError as e:
            raise ToolExecutionError(f"{tool_name} invalid JSON response: {e}")
        
        except Exception as e:
            raise ToolExecutionError(f"{tool_name} unexpected error: {e}")
    
    return tool_caller
