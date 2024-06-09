// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#pragma once

#include <map>
#include <memory>
#include <string>

#include "index/ScalarIndex.h"
#include "index/BitmapIndex.h"
#include "index/ScalarIndexSort.h"
#include "index/StringIndexMarisa.h"
#include "storage/FileManager.h"
#include "storage/DiskFileManagerImpl.h"
#include "storage/MemFileManagerImpl.h"
#include "storage/space.h"

namespace milvus {
namespace index {

enum class InternalIndexType {
    NONE = 0,
    BITMAP,
    STLSORT,
    MARISA,
};

/*
* @brief Implementation of hybrid index  
* @details This index only for scalar type.
* dynamically choose bitmap/stlsort/marisa type index
* according to data distribution
*/
template <typename T>
class HybridScalarIndex : public ScalarIndex<T> {
 public:
    explicit HybridScalarIndex(
        const storage::FileManagerContext& file_manager_context =
            storage::FileManagerContext());

    explicit HybridScalarIndex(
        const storage::FileManagerContext& file_manager_context,
        std::shared_ptr<milvus_storage::Space> space);

    ~HybridScalarIndex() override = default;

    BinarySet
    Serialize(const Config& config) override;

    void
    Load(const BinarySet& index_binary, const Config& config = {}) override;

    void
    Load(milvus::tracer::TraceContext ctx, const Config& config = {}) override;

    void
    LoadV2(const Config& config = {}) override;

    int64_t
    Count() override {
        return internal_index_->Count();
    }

    void
    Build(size_t n, const T* values) override {
        SelectIndexBuildType(n, values);
        auto index = GetInternalIndex();
        index->Build(n, values);
        is_built_ = true;
    }

    void
    Build(const Config& config = {}) override;

    void
    BuildV2(const Config& config = {}) override;

    const TargetBitmap
    In(size_t n, const T* values) override {
        return internal_index_->In(n, values);
    }

    const TargetBitmap
    NotIn(size_t n, const T* values) override {
        return internal_index_->NotIn(n, values);
    }

    const TargetBitmap
    Range(T value, OpType op) override {
        return internal_index_->Range(value, op);
    }

    const TargetBitmap
    Range(T lower_bound_value,
          bool lb_inclusive,
          T upper_bound_value,
          bool ub_inclusive) override {
        return internal_index_->Range(
            lower_bound_value, lb_inclusive, upper_bound_value, ub_inclusive);
    }

    T
    Reverse_Lookup(size_t offset) const override {
        return internal_index_->Reverse_Lookup(offset);
    }

    int64_t
    Size() override {
        return internal_index_->Size();
    }

    const bool
    HasRawData() const override {
        return internal_index_->HasRawData();
    }

    BinarySet
    Upload(const Config& config = {}) override;

    BinarySet
    UploadV2(const Config& config = {}) override;

 private:
    InternalIndexType
    SelectIndexBuildType(const std::vector<FieldDataPtr>& field_datas);

    InternalIndexType
    SelectIndexBuildType(size_t n, const T* values);

    void
    DeserializeIndexType(const BinarySet& binary_set);

    void
    BuildInternal(const std::vector<FieldDataPtr>& field_datas);

    void
    LoadInternal(const BinarySet& binary_set, const Config& config);

    std::shared_ptr<ScalarIndex<T>>
    GetInternalIndex();

 public:
    bool is_built_{false};
    int32_t bitmap_index_cardinality_limit_;
    InternalIndexType internal_index_type_;
    std::shared_ptr<ScalarIndex<T>> internal_index_{nullptr};
    std::shared_ptr<storage::MemFileManagerImpl> file_manager_{nullptr};
    std::shared_ptr<milvus_storage::Space> space_{nullptr};
};

}  // namespace index
}  // namespace milvus