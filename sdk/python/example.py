from openseal import workflow, task, trigger

@workflow(name="data_pipeline", description="Simple ETL pipeline")
class DataPipeline:
    @trigger.webhook(path="/ingest", method="POST")
    def ingest(self, event):
        return {"raw": event["data"]}

    @task.http(url="https://api.example.com/data", method="GET")
    def fetch(self, ingest_result):
        pass

    @task.transform(expr="{{ fetch.result | filter: active }}")
    def filter_active(self, fetch_result):
        pass

    @task.ai(
        prompt="Summarize these items: {{ filter_active.result }}",
        model="gpt-4o",
        provider="openai",
    )
    def summarize(self, filter_result):
        pass

    @task.http(url="https://api.dest.com/upload", method="POST")
    def save(self, summarize_result):
        pass
