use std::fs;

fn main() {
    let source = fs::read_to_string("/input/object").expect("read source object");
    fs::write("/output/uppercase.txt", source.to_uppercase()).expect("write derived object");
    print!(
        r#"{{"abi_version":"bucketmux.wasm.v1","metadata":{{"processed-by":"rust-uppercase"}},"tags":{{"transformed":"true"}},"derived_objects":[{{"path":"uppercase.txt","key_suffix":".uppercase.txt","content_type":"text/plain"}}]}}"#
    );
}
