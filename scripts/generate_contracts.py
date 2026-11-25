import os
import sys
import shutil
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PROTO_DIR = ROOT / "proto"
AI_GEN_DIR = ROOT / "ai-service" / "gen"

def generate_python_stubs():
    print(f"Generating Python gRPC stubs from {PROTO_DIR}...")
    AI_GEN_DIR.mkdir(parents=True, exist_ok=True)
    
    # Clean previous generated files if any
    for root, dirs, files in os.walk(AI_GEN_DIR):
        for f in files:
            if f.endswith(("_pb2.py", "_pb2_grpc.py")):
                os.remove(os.path.join(root, f))
                
    proto_files = list(PROTO_DIR.glob("**/*.proto"))
    if not proto_files:
        print("No proto files found!")
        return False

    cmd = [
        sys.executable,
        "-m",
        "grpc_tools.protoc",
        f"-I{PROTO_DIR}",
        f"--python_out={AI_GEN_DIR}",
        f"--grpc_python_out={AI_GEN_DIR}",
    ] + [str(p) for p in proto_files]
    
    print(f"Running command: {' '.join(cmd)}")
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print(f"Error generating Python stubs:\n{res.stderr}")
        return False
        
    # Ensure __init__.py exists in all generated subpackages
    for dirpath, dirnames, _ in os.walk(AI_GEN_DIR):
        dirnames[:] = [d for d in dirnames if d != "__pycache__"]
        init_file = Path(dirpath) / "__init__.py"
        init_file.touch(exist_ok=True)

    print("Python stubs generated successfully:")
    for f in AI_GEN_DIR.glob("**/*"):
        if f.is_file() and not f.name.endswith(".pyc"):
            print(f" - {f.relative_to(ROOT)}")
            
    return True

def generate_go_stubs():
    """Generates Go stubs with buf. Needs buf, protoc-gen-go and protoc-gen-go-grpc on PATH."""
    if shutil.which("buf") is None:
        print("buf not found on PATH; skipping Go stub generation")
        return True
    for tool in ("protoc-gen-go", "protoc-gen-go-grpc"):
        if shutil.which(tool) is None:
            print(f"{tool} not found on PATH; skipping Go stub generation")
            return True
    res = subprocess.run(["buf", "generate"], cwd=ROOT, capture_output=True, text=True)
    if res.returncode != 0:
        print("Error generating Go stubs:")
        print(res.stderr)
        return False
    print("Go stubs generated successfully")
    return True


if __name__ == "__main__":
    if not (generate_python_stubs() and generate_go_stubs()):
        sys.exit(1)
