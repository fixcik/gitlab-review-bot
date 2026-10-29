<?php

declare(strict_types=1);

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * @property int $id
 * @property string $created_by
 * @property string $status
 * @property string $total
 */
class Order extends Model
{
    protected $table = 'orders';

    protected $fillable = ['created_by', 'status', 'total'];
}
