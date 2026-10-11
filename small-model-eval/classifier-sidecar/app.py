import os
import time
from typing import List

from fastapi import FastAPI
from pydantic import BaseModel
from transformers import pipeline

MODEL = os.environ.get("ZSC_MODEL", "MoritzLaurer/deberta-v3-base-zeroshot-v1.1-all-33")

# device=-1 forces CPU so this never competes with the GPU that serves the agent model.
clf = pipeline("zero-shot-classification", model=MODEL, device=-1)

app = FastAPI(title="decision-sidecar")


class ClassifyRequest(BaseModel):
    text: str
    labels: List[str]
    multi_label: bool = False
    hypothesis_template: str = "This task should run in a {} environment."


@app.get("/health")
def health():
    return {"status": "ok", "model": MODEL}


@app.post("/classify")
def classify(r: ClassifyRequest):
    t0 = time.time()
    out = clf(
        r.text,
        candidate_labels=r.labels,
        multi_label=r.multi_label,
        hypothesis_template=r.hypothesis_template,
    )
    latency_ms = int((time.time() - t0) * 1000)
    ranking = [
        {"label": lbl, "score": round(float(sc), 4)}
        for lbl, sc in zip(out["labels"], out["scores"])
    ]
    return {
        "label": out["labels"][0],
        "score": round(float(out["scores"][0]), 4),
        "ranking": ranking,
        "latency_ms": latency_ms,
    }
