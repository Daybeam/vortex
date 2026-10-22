#!/bin/sh
# Pre-commit inspection script for git staging area cleanliness in Vortex

MAX_FILES=20
MAX_SIZE_BYTES=5242880 # 5MB

echo "==> [Vortex Hook] Running Git Staging Cleanliness Check..."

# 1. Count staged files
STAGED_COUNT=$(git diff --cached --name-only | wc -l | tr -d ' ')

if [ -n "$STAGED_COUNT" ] && [ "$STAGED_COUNT" -gt "$MAX_FILES" ]; then
    echo "[WARNING] Staged files count ($STAGED_COUNT) exceeds recommended maximum ($MAX_FILES)."
    echo "          Consider splitting changes into smaller, atomic commits."
fi

# 2. Check for unwanted file patterns
UNWANTED_STAGED=$(git diff --cached --name-only | grep -E "(_backup_|project_backup_|\.pyc$|\.docx$|\.tar\.gz$|\.exe$|\.idea/dbnavigator\.xml)")
if [ -n "$UNWANTED_STAGED" ]; then
    echo "[ERROR] Unwanted temporary or binary files detected in staged area:"
    echo "$UNWANTED_STAGED"
    echo "Please unstage these files before committing (e.g. git restore --staged <file>)."
    exit 1
fi

# 3. Check for oversized files (> 5MB)
OVERSIZED_FOUND=0
for file in $(git diff --cached --name-only); do
    if [ -f "$file" ]; then
        FILESIZE=$(wc -c <"$file" 2>/dev/null | tr -d ' ')
        if [ -n "$FILESIZE" ] && [ "$FILESIZE" -gt "$MAX_SIZE_BYTES" ]; then
            echo "[ERROR] Oversized file (>5MB) staged: $file (${FILESIZE} bytes)"
            OVERSIZED_FOUND=1
        fi
    fi
done

if [ "$OVERSIZED_FOUND" -eq 1 ]; then
    echo "Please unstage large binaries or archives, or use Git LFS."
    exit 1
fi

echo "==> [Vortex Hook] Git Staging Check Passed! ($STAGED_COUNT files staged)"
exit 0
