use std::io::{self, Read};

fn json_string(request: &str, key: &str) -> Option<String> {
    let marker = format!("\"{}\":\"", key);
    let start = request.find(&marker)? + marker.len();
    let rest = &request[start..];
    let end = rest.find('"')?;
    Some(rest[..end].to_string())
}

fn vector(raw: &str) -> Vec<f64> {
    raw.split(',').filter_map(|item| item.trim().parse().ok()).collect()
}

fn cosine(left: &[f64], right: &[f64]) -> f64 {
    if left.len() != right.len() || left.is_empty() {
        return 0.0;
    }
    let dot: f64 = left.iter().zip(right).map(|(a, b)| a * b).sum();
    let left_norm: f64 = left.iter().map(|v| v * v).sum::<f64>().sqrt();
    let right_norm: f64 = right.iter().map(|v| v * v).sum::<f64>().sqrt();
    if left_norm == 0.0 || right_norm == 0.0 { 0.0 } else { dot / (left_norm * right_norm) }
}

fn main() {
    let mut request = String::new();
    io::stdin().read_to_string(&mut request).expect("read invocation");
    let candidate = vector(&json_string(&request, "face_embedding").unwrap_or_default());
    let reference = vector(&json_string(&request, "reference_embedding").unwrap_or_default());
    let threshold = json_string(&request, "threshold").and_then(|v| v.parse().ok()).unwrap_or(0.8);
    let score = cosine(&candidate, &reference);
    let matched = if score >= threshold { "true" } else { "false" };
    print!(
        "{{\"abi_version\":\"bucketmux.wasm.v1\",\"metadata\":{{\"face-similarity\":\"{:.6}\"}},\"tags\":{{\"face-match\":\"{}\",\"matcher\":\"rust-cosine-demo-v1\"}}}}",
        score, matched
    );
}
