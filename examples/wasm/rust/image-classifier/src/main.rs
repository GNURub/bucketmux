use std::io::{self, Read};

fn main() {
    let mut request = String::new();
    io::stdin().read_to_string(&mut request).expect("read invocation");
    let category = if request.contains("\"content_type\":\"image/") {
        "image"
    } else if request.contains("\"content_type\":\"video/") {
        "video"
    } else {
        "other"
    };
    print!(
        "{{\"abi_version\":\"bucketmux.wasm.v1\",\"metadata\":{{\"classifier\":\"rust-rule-demo-v1\"}},\"tags\":{{\"media-category\":\"{}\"}}}}",
        category
    );
}
